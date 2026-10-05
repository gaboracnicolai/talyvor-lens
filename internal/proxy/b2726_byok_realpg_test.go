package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/byok"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/envelope"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// B27.26 — BYOK, the one subscription tier. A workspace on the BYOK plan that stores its own provider key is
// served on that key with no token charge; a pooled serve between two BYOK workspaces charges and mints
// nothing; a BYOK workspace's answer reused by a paying workspace mints the contributor its half. On a
// migrated schema through the real handler, buffered and streamed, asserted on lxc_ledger, agent_postings,
// pool_royalty_mints and lens_token_ledger — and on the Authorization header the provider received.

// b2726Upstream answers like OpenAI — streamed when the request asks — and records each Authorization header.
type b2726Upstream struct {
	mu    sync.Mutex
	auths []string
}

func (u *b2726Upstream) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		usage := `{"prompt_tokens":10000,"completion_tokens":100}`
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
				"data: {\"choices\":[],\"usage\":"+usage+"}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":`+usage+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (u *b2726Upstream) calls() (int, string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auths) == 0 {
		return 0, ""
	}
	return len(u.auths), u.auths[len(u.auths)-1]
}

// b2726BYOK puts ws on the BYOK plan (the row the webhook writes for a talyvor_byok_monthly Price) and stores
// its own OpenAI key.
func b2726BYOK(t *testing.T, pool *pgxpool.Pool, store *byok.Store, ws, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO subscriptions (workspace_id, stripe_subscription_id,
		stripe_customer_id, price_id, status, livemode, last_event_at, byok, plan)
		VALUES ($1, 'sub_' || $1, 'cus_' || $1, 'price_byok', 'active', false, NOW(), true, 'byok')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), ws, "openai", key); err != nil {
		t.Fatal(err)
	}
}

func b2726Store(t *testing.T, pool *pgxpool.Pool) *byok.Store {
	t.Helper()
	ring, err := envelope.NewKeyring(bytes.Repeat([]byte{7}, envelope.KEKLen))
	if err != nil {
		t.Fatal(err)
	}
	return byok.New(pool, ring)
}

func TestB2726_ABYOKQuestionGoesUpstreamOnItsOwnKeyWithNoTokenCharge(t *testing.T) {
	const ownKey = "sk-own-b2726-ws-log-0001"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			p, _, pool, agentID := b2313Setup(t, stream, 10000, 100, nil)
			up := &b2726Upstream{}
			p.openAIURL = up.serve(t)
			store := b2726Store(t, pool)
			p.SetOwnKeys(store)

			// The control: before BYOK the same kind of question goes on Talyvor's key and is charged.
			b2313Ask(t, p, 0, stream)
			rows0, net0, spent0, _ := b2313Books(t, pool, agentID)
			if n, auth := up.calls(); n != 1 || auth != "Bearer openai-key" || rows0 == 0 || spent0 == 0 {
				t.Fatalf("control: %d upstream calls on %q, %d spend rows, agent spent %d — want one call on Talyvor's key, charged",
					n, auth, rows0, spent0)
			}

			b2726BYOK(t, pool, store, b2313WS, ownKey)
			b2313Ask(t, p, 1, stream)
			if n, auth := up.calls(); n != 2 || auth != "Bearer "+ownKey {
				t.Errorf("BYOK question: %d upstream calls, the last on %q — want it sent on the workspace's own key", n, auth)
			}
			rows1, net1, spent1, holds1 := b2313Books(t, pool, agentID)
			if rows1 != rows0 || net1 != net0 || spent1 != spent0 || holds1 != b2313Fund-spent0 {
				t.Errorf("BYOK question was charged: lxc_ledger %d spend rows, net %d (were %d, %d); agent spent %d, holds %d (spent %d before)",
					rows1, net1, rows0, net0, spent1, holds1, spent0)
			}
		})
	}
}

// A BYOK request moves no LXC, so no hold judges it — its agent's rules still do: a model the agent may not
// use is refused before the provider is called.
func TestB2726_ABYOKAgentRequestIsStillHeldToItsAgentsRules(t *testing.T) {
	p, _, pool, _ := b2313Setup(t, false, 10000, 100, &economy.AgentRules{AllowedModels: []string{"gpt-4o-mini"}})
	up := &b2726Upstream{}
	p.openAIURL = up.serve(t)
	store := b2726Store(t, pool)
	p.SetOwnKeys(store)
	b2726BYOK(t, pool, store, b2313WS, "sk-own-b2726-ws-log-0001")

	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, b2313Content(0))
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: b2313Key, WorkspaceID: b2313WS}))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	if n, _ := up.calls(); w.Code != http.StatusForbidden || n != 0 {
		t.Errorf("status %d with %d upstream calls — want 403 before the provider is called; body=%s", w.Code, n, w.Body.String())
	}
}

func TestB2726_PooledServes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		consumerBYOK bool
	}{
		{"BYOK to BYOK mints nothing", true},
		{"a paying workspace reusing a BYOK answer mints the contributor's half", false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/streamed=%t", tc.name, stream), func(t *testing.T) {
				p, _, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
				up := &b2726Upstream{}
				p.openAIURL = up.serve(t)
				store := b2726Store(t, pool)
				p.SetOwnKeys(store)
				p.SetPoolConsumerDiscount(0.30)
				ctx := context.Background()
				for _, ws := range []string{"wsA", b2313WS} {
					if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix, earn_verified)
						VALUES ($1, $1, $1, true) ON CONFLICT (id) DO UPDATE SET earn_verified = true`, ws); err != nil {
						t.Fatal(err)
					}
				}
				ledger := mining.NewLedgerStore(pool)
				ledger.SetMintVerifier(earnverify.New(false))
				p.SetRoyaltyMinter(poolroyalty.NewMinter(pool, ledger, 0.5, func() bool { return true }))
				wsm := p.workspaceManager
				if err := wsm.RegisterWorkspace(ctx, workspace.Workspace{
					ID: "wsA", Name: "wsA", Active: true, LoggingPolicy: workspace.LoggingMetadata,
				}); err != nil {
					t.Fatal(err)
				}
				for _, ws := range []string{"wsA", b2313WS} {
					if err := wsm.SetCachePoolable(ctx, ws, true); err != nil {
						t.Fatal(err)
					}
				}
				p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))

				// wsA, on BYOK, asks first: answered on its own key, and the answer goes to the pool.
				b2726BYOK(t, pool, store, "wsA", "sk-own-b2726-wsA-0001")
				seed := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(
					fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, b2313Content(0))))
				seed.Header.Set("Content-Type", "application/json")
				seed.Header.Set("X-Talyvor-Workspace", "wsA")
				sw := httptest.NewRecorder()
				p.HandleOpenAI(sw, seed)
				if n, auth := up.calls(); sw.Code != http.StatusOK || n != 1 || auth != "Bearer sk-own-b2726-wsA-0001" {
					t.Fatalf("seed: status %d, %d upstream calls on %q — want 200 on wsA's own key", sw.Code, n, auth)
				}
				if tc.consumerBYOK {
					b2726BYOK(t, pool, store, b2313WS, "sk-own-b2726-ws-log-0001")
				}
				_, net0, spent0, _ := b2313Books(t, pool, agentID)

				w := b269Ask(t, p, stream)
				if n, _ := up.calls(); n != 1 {
					t.Fatalf("the consumer's question went upstream (%d calls) — it was not a pooled serve", n)
				}
				charged, _ := strconv.ParseInt(w.Header().Get("X-Talyvor-Pool-Charged-ULXC"), 10, 64)
				_, net1, spent1, _ := b2313Books(t, pool, agentID)
				var mints int
				var contributor string
				var minted int64
				if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(contributor_workspace_id), ''),
					COALESCE(sum(minted_amount), 0)::bigint FROM pool_royalty_mints`).Scan(&mints, &contributor, &minted); err != nil {
					t.Fatal(err)
				}
				var credited int64
				if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0)::bigint FROM lens_token_ledger
					WHERE workspace_id = 'wsA' AND type = $1`, mining.TypePoolRoyaltyHeld).Scan(&credited); err != nil {
					t.Fatal(err)
				}

				if tc.consumerBYOK {
					if charged != 0 || net1 != net0 || spent1 != spent0 || mints != 0 || credited != 0 {
						t.Errorf("BYOK to BYOK: charged %d (header), lxc_ledger net %d→%d, agent spent %d→%d; %d mints, wsA credited %d — want nothing charged or minted",
							charged, net0, net1, spent0, spent1, mints, credited)
					}
					if got := w.Header().Get("X-Talyvor-BYOK"); got != "own-key" {
						t.Errorf("X-Talyvor-BYOK = %q, want own-key", got)
					}
					return
				}
				if charged <= 0 || net1 != net0-charged || spent1 != spent0+charged {
					t.Fatalf("paying consumer: charged %d, lxc_ledger net %d→%d, agent spent %d→%d — want the discounted price charged",
						charged, net0, net1, spent0, spent1)
				}
				if mints != 1 || contributor != "wsA" || minted != charged/2 || credited != minted {
					t.Errorf("paying consumer: %d mints to %q of %d µLENS, wsA credited %d — want one mint of %d (half the %d charge) to wsA",
						mints, contributor, minted, credited, charged/2, charged)
				}
			})
		}
	}
}
