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

// B19.7 — ONE SWITCH PAUSES EVERY AGENT: the next request of each, buffered and streamed, is refused
// before the provider is called, and resuming restores them — all but an agent paused on its own.
func TestB197_PauseAllRefusesEveryAgentBeforeTheProvider_AndResumeRestoresThem(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.router = nil
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
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
	agents := map[string]string{}
	for _, key := range []string{"key-a", "key-b"} {
		a, err := store.CreateAgent(ctx, ws, key, "user-owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FundAgent(ctx, ws, a.ID, 20_000_000); err != nil {
			t.Fatal(err)
		}
		agents[key] = a.ID
	}
	ask := func(key string, stream bool) (int, string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":"gpt-4o","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", ws)
		req.Header.Set(CacheBypassHeader, "bypass")
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: ws}))
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w.Code, w.Body.String()
	}
	ledgerRows := func() (n int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	served := func(key string, stream bool) {
		t.Helper()
		calls := upstreamCalls.Load()
		if code, body := ask(key, stream); code != http.StatusOK || upstreamCalls.Load() != calls+1 {
			t.Errorf("%s (stream=%t) = %d %s with %d provider calls, want 200 and one call", key, stream, code, body, upstreamCalls.Load()-calls)
		}
	}
	refused := func(key string, stream bool, says string) {
		t.Helper()
		calls, rows := upstreamCalls.Load(), ledgerRows()
		code, body := ask(key, stream)
		if code != http.StatusForbidden || !strings.Contains(body, says) {
			t.Errorf("%s (stream=%t) = %d %s, want 403 saying %q", key, stream, code, body, says)
		}
		if upstreamCalls.Load() != calls || ledgerRows() != rows {
			t.Errorf("%s (stream=%t) was refused after the provider was called or something was charged", key, stream)
		}
	}

	for _, key := range []string{"key-a", "key-b"} {
		served(key, false)
	}
	if err := store.PauseAllAgents(ctx, ws, "incident 42"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"key-a", "key-b"} {
		for _, stream := range []bool{false, true} {
			refused(key, stream, "every agent in this workspace is paused (incident 42)")
		}
	}
	// B is paused on its own while everything is paused; resuming everything leaves it paused.
	if err := store.PauseAgent(ctx, ws, agents["key-b"], "under review"); err != nil {
		t.Fatal(err)
	}
	if err := store.ResumeAllAgents(ctx, ws); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		served("key-a", stream)
		refused("key-b", stream, "the agent is paused (under review)")
	}
}
