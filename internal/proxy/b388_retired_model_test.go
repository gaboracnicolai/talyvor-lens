package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B38.8 — a request for a model its provider retired is refused with a "model retired" error naming the
// documented successor, on both serve seams and under its dated snapshot id, and is never sent upstream —
// neither to the retired model nor, silently, to the successor.
func TestServe_RetiredModelIsRefusedNamingItsSuccessorAndNeverForwarded(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	p := newProxyWithFallback(t, upstream.URL, upstream.URL, upstream.URL)

	for _, body := range []string{
		`{"model":"claude-opus-4-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"claude-opus-4-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		`{"model":"claude-opus-4-1-20250805","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		w := newFlushRecorder()
		p.HandleAnthropic(w, httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", strings.NewReader(body)))
		var got struct{ Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if w.Code != http.StatusGone || !strings.HasPrefix(got.Error, "model retired: ") || !strings.Contains(got.Error, "use claude-opus-4-8") {
			t.Errorf("%s → %d %q, want 410 \"model retired: … use claude-opus-4-8\"", body, w.Code, w.Body.String())
		}
	}
	if hits != 0 {
		t.Errorf("a retired model's request reached the upstream %d times, want none", hits)
	}
}
