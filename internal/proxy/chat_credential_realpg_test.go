package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/sessionkey"
)

// B10.7 — the chat's credential, switched on (LENS_SESSION_KEYS_ENABLED forwarded, default true).
// A REAL minted session key through the REAL auth middleware and proxy scope gate: it is answered and
// billed to its own workspace (B9.8), naming another workspace in the header reaches nothing there,
// and once revoked it is refused before the provider is called.
func TestChatCredential_ThroughTheRealMiddleware(t *testing.T) {
	p, _, store, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
	seamFund(t, pool, "ws-other", costWireFunded)
	var calls int64
	chatUpstream(t, p, false, &calls)

	ctx := context.Background()
	keys := sessionkey.NewStore(pool)
	raw, sk, err := keys.Mint(ctx, "ws-log", "user-1", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	h := auth.AuthMiddleware(auth.New(pool), auth.NewManager("", nil, nil, nil).WithSessionKeys(keys))(
		auth.RequireScope(auth.ScopeProxy)(http.HandlerFunc(p.HandleOpenAI)))
	send := func(workspaceHeader, tag string) int {
		body := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, tag+" "+strings.Repeat("x", 40000))
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+raw)
		req.Header.Set("X-Talyvor-Workspace", workspaceHeader)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	cost := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))

	if code := send("ws-log", "b107-own"); code != http.StatusOK {
		t.Fatalf("session key on its own workspace: status %d, want 200", code)
	}
	if rows, ulxc, desc := prepaidDebits(t, pool); rows != 1 || ulxc != cost || desc != "chat: metered usage" {
		t.Errorf("billing: %d row(s), %d µLXC, %q; want 1, %d, \"chat: metered usage\"", rows, ulxc, desc, cost)
	}

	if code := send("ws-other", "b107-other"); code != http.StatusOK {
		t.Fatalf("session key naming another workspace: status %d, want 200 (served as its own)", code)
	}
	if bal := seamBalance(t, store, "ws-other"); bal != costWireFunded {
		t.Errorf("ws-other balance = %d, want %d untouched — a session key reached another workspace", bal, costWireFunded)
	}
	if rows, ulxc, _ := prepaidDebits(t, pool); rows != 2 || ulxc != 2*cost {
		t.Errorf("ws-log billing after both: %d row(s), %d µLXC; want 2, %d", rows, ulxc, 2*cost)
	}

	if err := keys.Revoke(ctx, "ws-log", sk.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if code := send("ws-log", "b107-revoked"); code != http.StatusUnauthorized {
		t.Errorf("revoked session key: status %d, want 401", code)
	}
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Errorf("upstream calls = %d, want 2 — the revoked request must not reach the provider", n)
	}
}
