package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// CACHE-SERVE SPEND VISIBILITY (0099): a cache-served response must write a token_events row
// tagged with its serve layer so the cache hit rate — the trial's core number — is countable from
// the same table every other spend reader uses. Before this change the cache serve points returned
// before the recording seam: the requester was debited (pre-serve LXC estimate) but the request was
// invisible to /v1/api/spend/by-request. Billed, invisible.
//
// These sink-level tests pin the proxy-side contract (which serve point calls which sink method,
// with which tag); the row's persisted field values are pinned against real PG in
// internal/alerts/cache_serve_realpg_test.go and end-to-end in cache_serve_visibility_realpg_test.go.

// dispatchStream posts the same prompt as dispatch() but with "stream": true so a warmed cache
// entry is served through the SSE-replay serve point rather than writeBytes.
func dispatchStream(t *testing.T, p *Proxy, wsID string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

// An exact cache hit (the writeBytes serve point) must record exactly one cache-tagged spend row —
// cost estimated, tokens measured — while the preceding miss keeps its unchanged upstream row.
func TestCacheServe_ExactHit_RecordsTaggedSpendRow(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	if w := dispatch(t, p, "ws-log"); w.Code != http.StatusOK { // miss → upstream
		t.Fatalf("miss dispatch: status %d, body=%s", w.Code, w.Body.String())
	}
	if w := dispatch(t, p, "ws-log"); w.Code != http.StatusOK { // identical → exact hit
		t.Fatalf("hit dispatch: status %d, body=%s", w.Code, w.Body.String())
	}

	sink.mu.Lock()
	calls := sink.calls
	sink.mu.Unlock()
	if calls != 2 {
		t.Fatalf("spend writes = %d, want 2 (one upstream miss + one tagged cache hit)", calls)
	}
	if _, ok := sink.spendWithServeSource(""); !ok {
		t.Error("missing the upstream (untagged) spend row for the miss")
	}
	hit, ok := sink.spendWithServeSource("cache_hit_exact")
	if !ok {
		t.Fatal("missing the cache_hit_exact spend row for the cache-served request")
	}
	if !hit.estimated {
		t.Error("cache row estimated = false, want true (no provider usage exists on a cache serve)")
	}
	if hit.inputTokens <= 0 || hit.outputTokens <= 0 {
		t.Errorf("cache row tokens = %d/%d, want both > 0 (the tokens Lens measured)", hit.inputTokens, hit.outputTokens)
	}
}

// The SSE-replay serve point is the same seam for streaming clients: a warmed entry replayed as
// SSE must record the same tagged row.
func TestCacheServe_StreamingReplay_RecordsTaggedSpendRow(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	if w := dispatch(t, p, "ws-log"); w.Code != http.StatusOK { // warm the cache (non-streaming miss)
		t.Fatalf("warm dispatch: status %d, body=%s", w.Code, w.Body.String())
	}
	if w := dispatchStream(t, p, "ws-log"); w.Code != http.StatusOK { // streaming request → SSE replay
		t.Fatalf("stream dispatch: status %d, body=%s", w.Code, w.Body.String())
	}

	if _, ok := sink.spendWithServeSource("cache_hit_exact"); !ok {
		t.Fatal("missing the cache_hit_exact spend row for the SSE-replayed cache serve")
	}
}

// LoggingNone opts a workspace out of every per-request observability write — the cache row
// included, symmetric with the upstream recording seam and the streaming seam.
func TestCacheServe_LoggingNone_WritesNothing(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingNone)

	if w := dispatch(t, p, "ws-log"); w.Code != http.StatusOK {
		t.Fatalf("miss dispatch: status %d", w.Code)
	}
	if w := dispatch(t, p, "ws-log"); w.Code != http.StatusOK {
		t.Fatalf("hit dispatch: status %d", w.Code)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.calls != 0 {
		t.Errorf("LoggingNone: spend writes = %d, want 0 (cache hits respect the opt-out too)", sink.calls)
	}
}
