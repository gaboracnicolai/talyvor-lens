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

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B28.302 — THE 61ST REQUEST IN A MINUTE UNDER A 60/MIN RULE WRITES NO HOLD, BUFFERED OR STREAMED.
//
// The real handler with the agent reservation on and a migrated schema: the owner's 60-a-minute rule lets sixty
// questions through to the provider, each held; the next is answered 429 before the provider is called, and the
// agent's book has no hold for it.
func TestB28302_The61stRequestInAMinuteUnderA60PerMinuteRuleWritesNoHold(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 256 })
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`)
	}))
	t.Cleanup(upstream.Close)
	p.openAIURL = upstream.URL

	const ws = "ws-log"
	seamFund(t, pool, ws, 100_000_000)
	agent, err := store.CreateAgent(ctx, ws, "chatty", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, ws, agent.ID, "key-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, ws, agent.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	sixty := int64(60)
	if _, err := store.SetAgentRules(ctx, ws, agent.ID, economy.AgentRules{RequestsPerMinute: &sixty}); err != nil {
		t.Fatal(err)
	}

	ask := func(i int, stream bool) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":"gpt-4o-mini","stream":%t,"messages":[{"role":"user","content":"question %d"}]}`, stream, i)
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-agent", WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w.Code, w.Body.String()
	}
	holds := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE workspace_id = $1 AND kind = 'hold'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for i := 1; i <= 60; i++ {
		if code, body := ask(i, false); code != http.StatusOK {
			t.Fatalf("request %d of 60 in a minute = %d %s, want 200", i, code, body)
		}
	}
	if n := holds(); n != 2*60 { // a hold is two postings: the agent's side and the spend side
		t.Fatalf("sixty requests wrote %d hold postings, want %d", n, 2*60)
	}

	for _, stream := range []bool{false, true} {
		calls, before := upstreamCalls.Load(), holds()
		code, body := ask(61, stream)
		if code != http.StatusTooManyRequests || !strings.Contains(body, "60 requests a minute") {
			t.Errorf("the 61st request in a minute (stream=%t) = %d %s, want 429 naming the 60-a-minute rule", stream, code, body)
		}
		if n := holds(); n != before {
			t.Errorf("the 61st request in a minute (stream=%t) wrote %d hold postings, want none", stream, n-before)
		}
		if upstreamCalls.Load() != calls {
			t.Errorf("the 61st request in a minute (stream=%t) reached the provider", stream)
		}
	}
}
