package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/billing"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/sessionkey"
	"github.com/talyvor/lens/migrations"
)

// session_key_billing_realpg_test.go — B9.8: every browser-chat request is charged.
//
// This file used to MEASURE that a session-key request moved no LXC at all (W4.6.1 step 4b,
// docs/model2-step4b-session-billing-measured.md): the chat credential carries no APIKeyID, so it
// never reached the agent reservation, the only thing that billed by default. Its tests were written
// to go red when that changed. B9.8 is the change (chat_billing.go), so they are replaced by what a
// chat request now books, asserted on the ledger rows, on BOTH seams, through the real handler, with
// the AuthContext built by the real auth.Manager:
//
//	subscriber within the allowance → the allowance pays it all, no prepaid row
//	subscriber past the allowance    → the whole allowance, then one prepaid row for the rest
//	non-subscriber                   → one prepaid row for the whole cost
//
// and, before the provider is called: no credit → 402; a session at its bound → 402.

// stubSessionKeyValidator satisfies internal/auth's session-key validator seam.
//
// ⚠ THE AuthContext UNDER TEST IS BUILT BY THE PRODUCT, NOT BY THIS FILE. A hand-written
// &auth.AuthContext{APIKeyID: ""} would make "the session credential carries no key id" an
// assumption of the fixture rather than a fact about internal/auth — and this repo has already
// measured what a fixture more permissive than the product proves (nothing). So the arm below runs
// the REAL auth.Manager.Authenticate and uses whatever AuthContext it returns.
type stubSessionKeyValidator struct{ sk *sessionkey.SessionKey }

func (s stubSessionKeyValidator) Validate(context.Context, string) (*sessionkey.SessionKey, error) {
	return s.sk, nil
}

// sessionKeyAuthContext returns the AuthContext the REAL auth.Manager produces for a session key.
func sessionKeyAuthContext(t *testing.T, wsID string) *auth.AuthContext {
	t.Helper()
	raw := sessionkey.KeyPrefix + "measured0000000000000000000000000"
	mgr := auth.NewManager("", nil, nil, nil).WithSessionKeys(stubSessionKeyValidator{sk: &sessionkey.SessionKey{
		ID:          testSessionKeyID,
		WorkspaceID: wsID,
		UserID:      "user-1",
		ExpiresAt:   time.Now().Add(time.Hour),
	}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	actx, err := mgr.Authenticate(req)
	if err != nil {
		t.Fatalf("the real auth.Manager refused a session key: %v — this measurement depends on it accepting one", err)
	}
	if actx.AuthMethod != auth.MethodSessionKey {
		t.Fatalf("AuthMethod = %q, want %q — the arm below would not be measuring a session key",
			actx.AuthMethod, auth.MethodSessionKey)
	}
	return actx
}

// driveWithAuth runs one request through the real handler under the given AuthContext.
// stream selects the seam: Lens's streaming lane is a second independent copy, so a finding proved
// on one seam says nothing about the other.
//
// ⚠ tag MAKES THE PROMPT UNIQUE PER ARM, AND IT IS LOAD-BEARING RATHER THAN TIDY. The first version
// sent a byte-identical body from both arms, so the second arm was a CACHE HIT: it never called
// upstream, released its hold and billed nothing — and the anti-blindness assertion caught it,
// reporting "THE HARNESS CANNOT SEE A BILL". Arm 1's zeros had been meaningless. Two arms that
// share a cache are one arm run twice.
func driveWithAuth(t *testing.T, p *Proxy, actx *auth.AuthContext, stream bool, tag string) int {
	t.Helper()
	prompt := tag + " " + strings.Repeat("x", 40000)
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]%s}`,
		prompt, map[bool]string{true: `,"stream":true`, false: ""}[stream])
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	req = req.WithContext(auth.WithAuthContext(req.Context(), actx))
	if stream {
		w := newFlushRecorder()
		p.HandleOpenAI(w, req)
		return w.Code
	}
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w.Code
}

// countingUpstream wraps an upstream and counts calls.
//
// ⚠ WITHOUT THIS, "NOTHING WAS BILLED" AND "NOTHING HAPPENED" ARE THE SAME OBSERVATION. A request
// short-circuited by the cache, or refused before the upstream call, also bills nothing — and would
// satisfy every zero-assertion below while proving nothing about the credential.
func countingUpstream(t *testing.T, inner *httptest.Server, n *int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(n, 1)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL, r.Body)
		if err != nil {
			t.Errorf("counting upstream: %v", err)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("counting upstream: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const testSessionKeyID = "00000000-0000-4000-8000-0000000000b9"

// chatProxy is costWireProxy (reservations on, shadow debit off: the default) with a real allowance
// service and a real session-key row, wired the way cmd/lens wires them. allowance 0 = no plan.
func chatProxy(t *testing.T, prepaid, allowance, bound int64) (*Proxy, *billing.Service, *economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	p, store, pool := costWireProxy(t)
	ctx := context.Background()
	for _, f := range []string{"0121_subscription_allowance.sql", "0127_subscription_allowance_fee.sql",
		"0122_session_keys.sql", "0128_session_key_spend.sql"} {
		ddl, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	for _, q := range []string{`DELETE FROM subscription_allowance WHERE workspace_id = 'ws-log'`,
		`DELETE FROM session_keys WHERE id = '` + testSessionKeyID + `'`,
		`INSERT INTO session_keys (id, workspace_id, user_id, key_hash, key_prefix, expires_at)
		 VALUES ('` + testSessionKeyID + `', 'ws-log', 'user-1', 'h-b98', 'tlv_sk_', now() + interval '1 hour')`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	seamFund(t, pool, "ws-log", prepaid)
	svc := billing.New(pool, store, nil, "").WithAllowance(allowance)
	if allowance > 0 {
		now := time.Now()
		if _, err := svc.Grant(ctx, "ws-log", "sub_b98", now.Add(-time.Hour), now.Add(30*24*time.Hour)); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	p.SetLXCSpendSink(store, func() bool { return false })
	p.SetLXCGate(store, func() bool { return false })
	p.SetSubscriptionAllowance(svc)
	p.SetSessionSpend(sessionkey.NewStore(pool), bound)
	return p, svc, store, pool
}

func prepaidDebits(t *testing.T, pool *pgxpool.Pool) (rows int, ulxc int64, desc string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*), COALESCE(SUM(-amount),0), COALESCE(MAX(description),'')
		FROM lxc_ledger WHERE workspace_id='ws-log' AND amount < 0`).Scan(&rows, &ulxc, &desc); err != nil {
		t.Fatal(err)
	}
	return rows, ulxc, desc
}

func sessionSpent(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT spent_ulxc FROM session_keys WHERE id=$1`, testSessionKeyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChatBilling_EveryChatRequestIsCharged_BothSeams(t *testing.T) {
	cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100)) // the upstream's reported usage
	cases := []struct {
		name          string
		allowance     int64
		wantAllowance int64 // consumed from the plan
		wantPrepaid   int64 // debited from prepaid, in one row
		wantDesc      string
	}{
		{"subscriber within the allowance", 10 * cost, cost, 0, ""},
		{"subscriber past the allowance", testAllowanceULXC, testAllowanceULXC, cost - testAllowanceULXC, "subscription: usage beyond the plan allowance"},
		{"non-subscriber", 0, 0, cost, "chat: metered usage"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "buffered", true: "streamed"}[stream], func(t *testing.T) {
				p, svc, store, pool := chatProxy(t, costWireFunded, tc.allowance, economy.DefaultAgentCeilingLXC)
				var calls int64
				chatUpstream(t, p, stream, &calls)
				if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b98-"+tc.name); code != http.StatusOK {
					t.Fatalf("status = %d, want 200", code)
				}
				if atomic.LoadInt64(&calls) != 1 {
					t.Fatalf("upstream calls = %d, want 1", calls)
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
				rows, debited, desc := prepaidDebits(t, pool)
				wantRows := 0
				if tc.wantPrepaid > 0 {
					wantRows = 1
				}
				if rows != wantRows || debited != tc.wantPrepaid || (wantRows == 1 && desc != tc.wantDesc) {
					t.Errorf("prepaid ledger = %d row(s), %d µLXC, %q; want %d, %d µLXC, %q", rows, debited, desc, wantRows, tc.wantPrepaid, tc.wantDesc)
				}
				if bal := seamBalance(t, store, "ws-log"); bal != costWireFunded-tc.wantPrepaid {
					t.Errorf("balance = %d, want %d", bal, costWireFunded-tc.wantPrepaid)
				}
				if got := sessionSpent(t, pool); got != cost {
					t.Errorf("session spent = %d µLXC, want the whole cost %d", got, cost)
				}
			})
		}
	}
}

// Refused BEFORE the provider is called: a non-subscriber with no credit, and a session at its bound.
func TestChatBilling_RefusedBeforeTheProvider_BothSeams(t *testing.T) {
	const bound = int64(1_000_000) // 1 LXC; this request's conservative estimate is well under it
	for _, stream := range []bool{false, true} {
		seam := map[bool]string{false: "buffered", true: "streamed"}[stream]
		t.Run("no credit/"+seam, func(t *testing.T) {
			p, _, _, _ := chatProxy(t, 0, 0, bound)
			var calls int64
			chatUpstream(t, p, stream, &calls)
			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b98-nocredit"); code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", code)
			}
			if n := atomic.LoadInt64(&calls); n != 0 {
				t.Fatalf("upstream calls = %d, want 0", n)
			}
		})
		t.Run("session at its bound/"+seam, func(t *testing.T) {
			p, _, _, pool := chatProxy(t, costWireFunded, 0, bound)
			var calls int64
			chatUpstream(t, p, stream, &calls)
			// Control: the same fresh session is admitted, so the refusal below is the bound's.
			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b98-under"); code != http.StatusOK {
				t.Fatalf("fresh session: status = %d, want 200", code)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE session_keys SET spent_ulxc = $2 WHERE id = $1`, testSessionKeyID, bound-1); err != nil {
				t.Fatal(err)
			}
			if code := driveWithAuth(t, p, sessionKeyAuthContext(t, "ws-log"), stream, "b98-over"); code != http.StatusPaymentRequired {
				t.Fatalf("session at its bound: status = %d, want 402", code)
			}
			if n := atomic.LoadInt64(&calls); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (the control only)", n)
			}
		})
	}
}
