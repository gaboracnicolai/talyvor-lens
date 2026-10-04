package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/internal/tenant"
	"github.com/talyvor/lens/internal/workspace"
)

// B27.2 — A QUESTION ANSWERED BY A NODE OR BY LOCAL ROUTING IS CHARGED AND COUNTED LIKE ANY OTHER.
//
// Both paths settled only an agent's hold (B26.5), so a chat or subscriber question they answered was
// free and invisible to budgets and spending limits. A migrated schema, the real handler, a node (or a
// local model) that answers, and an upstream that fails the test if it is called. Asserted on lxc_ledger,
// the allowance row, the session's total and the token_events row the budgets and the spending cap sum.

const b272Prompt = "b272 what is two plus two? "

// b272Proxy is the handler wired as cmd/lens wires billing (reservations on, shadow debit off) on a fully
// migrated schema, with ws-log funded, an allowance of `allowance` µLXC (0 = no plan), the chat session
// row and its bound, and the spending cap's own reader.
func b272Proxy(t *testing.T, allowance int64) (*Proxy, *billing.Service, *pgxpool.Pool) {
	t.Helper()
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	if err := p.workspaceManager.RegisterWorkspace(ctx, workspace.Workspace{
		ID: "default", Name: "default", Active: true, LoggingPolicy: workspace.LoggingFull,
	}); err != nil {
		t.Fatal(err)
	}
	p.router = nil
	p.setAlertSink(alerts.New(pool, nil, nil))
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	tstore := tenant.NewStore(pool)
	p.SetWorkspaceLimits(tstore, tenant.NewSpendTracker(tstore))
	for _, ws := range []string{"ws-log", "default"} {
		seamFund(t, pool, ws, costWireFunded)
		if _, err := pool.Exec(ctx, `INSERT INTO session_keys (id, workspace_id, user_id, key_hash, key_prefix, expires_at)
			VALUES ($1, $2, 'user-1', $3, 'tlv_sk_', now() + interval '1 hour')`, b272SessionKey(ws), ws, "h-b272-"+ws); err != nil {
			t.Fatal(err)
		}
	}
	svc := billing.New(pool, store, nil, "").WithAllowance(allowance)
	if allowance > 0 {
		now := time.Now()
		if _, err := svc.Grant(ctx, "ws-log", "sub_b272", now.Add(-time.Hour), now.Add(30*24*time.Hour)); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	p.SetSubscriptionAllowance(svc)
	p.SetSessionSpend(sessionkey.NewStore(pool), economy.DefaultAgentCeilingLXC)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the upstream was called — the question was not served off the upstream seams")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"upstream"}}]}`)
	}))
	t.Cleanup(upstream.Close)
	p.openAIURL = upstream.URL
	return p, svc, pool
}

func b272SessionKey(ws string) string {
	if ws == "default" {
		return "00000000-0000-4000-8000-0000000b2720"
	}
	return "00000000-0000-4000-8000-0000000b2721"
}

// b272Chat is the AuthContext the browser chat carries: a session key, no API key id.
func b272Chat(ws string) *auth.AuthContext {
	return &auth.AuthContext{WorkspaceID: ws, UserID: "user-1", AuthMethod: auth.MethodSessionKey, SessionKeyID: b272SessionKey(ws)}
}

// b272Drive sends one question through the real handler and returns the response.
func b272Drive(t *testing.T, p *Proxy, ws string, actx *auth.AuthContext, stream bool, requestID, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]%s}`,
		prompt, map[bool]string{true: `,"stream":true`, false: ""}[stream])
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", ws)
	req.Header.Set("X-Talyvor-Request-ID", requestID)
	req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

// b272Row is the token_events row of one request: what the budgets and the spending cap sum.
func b272Row(t *testing.T, pool *pgxpool.Pool, requestID string) (source, model string, costUSD float64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(serve_source, ''), model, cost_usd FROM token_events WHERE request_id = $1`, requestID).
		Scan(&source, &model, &costUSD); err != nil {
		t.Fatalf("exactly one token_events row for %s: %v", requestID, err)
	}
	return source, model, costUSD
}

func TestB272_NodeServedQuestionIsChargedAndCounted(t *testing.T) {
	const nodeText = "two plus two is four, as the node computed"
	prompt := b272Prompt + strings.Repeat("x", 40000) // ~10k tokens: the charge exceeds the test allowance
	usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, len(prompt)/4, 0, 0, len(nodeText)/4)
	cost := settleULXC(usd)
	if cost <= testAllowanceULXC {
		t.Fatalf("fixture: cost %d must exceed the allowance %d to exercise the prepaid remainder", cost, testAllowanceULXC)
	}
	for _, tc := range []struct {
		name          string
		allowance     int64
		actx          *auth.AuthContext
		wantAllowance int64  // drawn from the plan
		wantPrepaid   int64  // debited from prepaid, in one row
		wantDesc      string // the row the upstream seam writes for this question
		wantSession   int64
	}{
		{"chat question", 0, b272Chat("ws-log"), 0, cost, "chat: metered usage", cost},
		{"subscriber question", testAllowanceULXC, &auth.AuthContext{WorkspaceID: "ws-log", UserID: "user-1", AuthMethod: auth.MethodJWT},
			testAllowanceULXC, cost - testAllowanceULXC, "subscription: usage beyond the plan allowance", 0},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
				p, svc, pool := b272Proxy(t, tc.allowance)
				node := fakeInferenceNode(t, nodeText, 3, 9)
				wireNode(t, p, "gpt-4o", node.URL, node.Client())
				reqID := fmt.Sprintf("b272-node-%s-%v", strings.ReplaceAll(tc.name, " ", "-"), stream)

				w := b272Drive(t, p, "ws-log", tc.actx, stream, reqID, prompt)
				if w.Code != http.StatusOK || w.Header().Get("X-Talyvor-Node-Served") == "" {
					t.Fatalf("status %d, node %q — want 200 served by the node; body=%s", w.Code, w.Header().Get("X-Talyvor-Node-Served"), w.Body.String())
				}

				var rows int
				var debited int64
				var desc string
				if err := pool.QueryRow(context.Background(), `SELECT COUNT(*), COALESCE(SUM(-amount),0), COALESCE(MAX(description),'')
					FROM lxc_ledger WHERE workspace_id = 'ws-log' AND amount < 0`).Scan(&rows, &debited, &desc); err != nil {
					t.Fatal(err)
				}
				if rows != 1 || debited != tc.wantPrepaid || desc != tc.wantDesc {
					t.Errorf("prepaid ledger = %d row(s), %d µLXC, %q; want 1, %d µLXC, %q (before B27.2: none — the question was free)",
						rows, debited, desc, tc.wantPrepaid, tc.wantDesc)
				}
				if tc.allowance > 0 {
					a, err := svc.CurrentAllowance(context.Background(), "ws-log", time.Now())
					if err != nil || a == nil {
						t.Fatalf("allowance row = %+v, %v", a, err)
					}
					if a.ConsumedULXC != tc.wantAllowance {
						t.Errorf("allowance consumed = %d, want %d", a.ConsumedULXC, tc.wantAllowance)
					}
				}
				var spent int64
				if err := pool.QueryRow(context.Background(), `SELECT spent_ulxc FROM session_keys WHERE id = $1`, b272SessionKey("ws-log")).Scan(&spent); err != nil {
					t.Fatal(err)
				}
				if spent != tc.wantSession {
					t.Errorf("chat session spent = %d µLXC, want %d", spent, tc.wantSession)
				}

				// Budgets and the spending cap both sum token_events.cost_usd; the node row must carry the charge.
				source, _, rowUSD := b272Row(t, pool, reqID)
				if source != "node" || settleULXC(rowUSD) != cost {
					t.Errorf("token_events row = %q at $%v (%d µLXC), want 'node' at the charge %d µLXC — budgets and limits never see it",
						source, rowUSD, settleULXC(rowUSD), cost)
				}
				month, err := tenant.NewSpendTracker(tenant.NewStore(pool)).CurrentSpend(context.Background(), "ws-log")
				if err != nil {
					t.Fatal(err)
				}
				if settleULXC(month) != cost {
					t.Errorf("the spending cap reads $%v this month, want the charge (%d µLXC)", month, cost)
				}
			})
		}
	}
}

func TestB272_LocallyServedQuestionWritesAZeroCostRow(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
			p, _, pool := b272Proxy(t, 0)
			p.localRouter = fakeOllama(t) // local routing serves the default workspace only
			reqID := fmt.Sprintf("b272-local-%v", stream)

			w := b272Drive(t, p, "default", b272Chat("default"), stream, reqID, b272Prompt)
			if w.Code != http.StatusOK || w.Header().Get("X-Talyvor-Local-Model") == "" {
				t.Fatalf("status %d, local model %q — want 200 served locally; body=%s", w.Code, w.Header().Get("X-Talyvor-Local-Model"), w.Body.String())
			}
			_, model, rowUSD := b272Row(t, pool, reqID)
			if model != "llama3.2:latest" || rowUSD != 0 {
				t.Errorf("spend row = %q at $%v, want the local model at exactly $0", model, rowUSD)
			}
			var rows int
			if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM lxc_ledger WHERE workspace_id = 'default' AND amount < 0`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Errorf("%d prepaid debit(s) for a local run — it costs Talyvor nothing and is charged 0", rows)
			}
		})
	}
}

// A node decides how long its answer is: a padded answer is charged no more output than the bound a
// request is admitted on.
func TestB272_NodeAnswerLengthIsChargedUpToTheOutputBound(t *testing.T) {
	p, _, pool := b272Proxy(t, 0)
	nodeText := strings.Repeat("padding ", 20_000) // 40k tokens, far past the 4096 bound
	node := fakeInferenceNode(t, nodeText, 3, 9)
	wireNode(t, p, "gpt-4o", node.URL, node.Client())

	if w := b272Drive(t, p, "ws-log", b272Chat("ws-log"), false, "b272-node-padded", b272Prompt); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	usd, _ := alerts.CostUSDResolved("gpt-4o", catalog.PurposeCharge, len(b272Prompt)/4, 0, 0, 4096)
	var debited int64
	if err := pool.QueryRow(context.Background(), `SELECT COALESCE(SUM(-amount),0) FROM lxc_ledger
		WHERE workspace_id = 'ws-log' AND description = 'chat: metered usage'`).Scan(&debited); err != nil {
		t.Fatal(err)
	}
	if want := settleULXC(usd); debited != want {
		t.Errorf("charged %d µLXC, want %d — the output bound's worth, not the node's padding", debited, want)
	}
}
