package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/workspace"
)

// B26.9 — WITH RESERVATIONS OFF, AN ANSWER FROM CACHE IS CHARGED ITS REAL PRICE.
//
// The pre-serve debit charges the input-only estimate; a cache serve then settles it like B23.13 settles a
// model serve: an own-cache hit to nothing, a pooled hit to its discounted price. B23.13's agent on a migrated
// schema, the real handler, reservations off. Asserted on lxc_ledger and agent_postings.

func TestB269_WithReservationsOff_AnOwnCacheHitCostsTheAgentNothing(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			p, _, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
			b2313Ask(t, p, 0, false) // a miss, charged its delivered cost, and kept
			_, netBefore, spentBefore, holdsBefore := b2313Books(t, pool, agentID)

			b2313Ask(t, p, 0, stream) // the same question, answered from the agent's own cache
			_, net, spent, holds := b2313Books(t, pool, agentID)
			if net != netBefore || spent != spentBefore || holds != holdsBefore {
				t.Errorf("the cache hit cost the agent: ledger net %d→%d, agent spent %d→%d, holds %d→%d — want no change",
					netBefore, net, spentBefore, spent, holdsBefore, holds)
			}
		})
	}
}

func TestB269_WithReservationsOff_APooledHitCostsItsDiscountedPrice(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed=%t", stream), func(t *testing.T) {
			p, _, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
			p.SetPoolConsumerDiscount(0.30)
			wsm := p.workspaceManager
			if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
				ID: "wsA", Name: "wsA", Active: true, LoggingPolicy: workspace.LoggingMetadata,
			}); err != nil {
				t.Fatal(err)
			}
			for _, ws := range []string{"wsA", b2313WS} {
				if err := wsm.SetCachePoolable(context.Background(), ws, true); err != nil {
					t.Fatal(err)
				}
			}
			p.SetPoolGate(cache_pooling.New(func() bool { return true }, wsm.GetCachePoolable))

			// Another workspace asks first and contributes the answer to the pool.
			seed := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(
				fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, b2313Content(0))))
			seed.Header.Set("Content-Type", "application/json")
			seed.Header.Set("X-Talyvor-Workspace", "wsA")
			sw := httptest.NewRecorder()
			p.HandleOpenAI(sw, seed)
			if sw.Code != http.StatusOK {
				t.Fatalf("seed status = %d, body=%s", sw.Code, sw.Body.String())
			}

			w := b269Ask(t, p, stream)
			charged, _ := strconv.ParseInt(w.Header().Get("X-Talyvor-Pool-Charged-ULXC"), 10, 64)
			list, _ := strconv.ParseInt(w.Header().Get("X-Talyvor-Pool-List-ULXC"), 10, 64)
			if charged <= 0 || charged >= list {
				t.Fatalf("not a discounted pooled serve: charged %d, list %d", charged, list)
			}
			spendRows, net, spent, holds := b2313Books(t, pool, agentID)
			if spendRows != 2 || net != -charged || spent != charged || holds != b2313Fund-charged {
				t.Errorf("lxc_ledger: %d spend rows, net %d; agent spent %d, holds %d — want the estimate and one settling row, %d charged, %d left",
					spendRows, net, spent, holds, charged, b2313Fund-charged)
			}
			var raw []byte
			if err := pool.QueryRow(context.Background(), `SELECT metadata FROM lxc_ledger
				WHERE workspace_id = $1 AND type = 'spend' ORDER BY created_at DESC LIMIT 1`, b2313WS).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var meta struct {
				List  int64 `json:"pool_list_ulxc"`
				Saved int64 `json:"pool_saved_ulxc"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			if meta.List != list || meta.Saved != list-charged {
				t.Errorf("settling row says list %d, saved %d — want %d, %d", meta.List, meta.Saved, list, list-charged)
			}
		})
	}
}

// b269Ask is the agent's question-0, returning the response so its pooled-price headers can be read.
func b269Ask(t *testing.T, p *Proxy, stream bool) *flushRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}],"stream":%t}`, b2313Content(0), stream)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req.Header.Set("X-Talyvor-Request-ID", "question-0")
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: b2313Key, WorkspaceID: b2313WS}))
	w := newFlushRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}
