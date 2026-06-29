package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/retry"
)

// CHARACTERIZATION TEST (PR-3b step a2) — pins the CURRENT same-provider retry-then-success behavior of
// forward (proxy.go:1883). Recon flagged this branch as untested. forward wraps the upstream call in
// retry.Do(p.retryConfig, …): a retryable status (in RetryableCodes) re-sends to the SAME provider until
// MaxAttempts; a success returns immediately (NO fallback — this is the single-provider forward, not
// forwardWithFallback). retry.Do sets result.Attempts = the attempt number reached, which forward returns.
//
// When forward's round-trip body moves to internal/inference in step (b), THIS test stays in package proxy
// as the behavior oracle — a verbatim move must keep the recovery + attempt-count + re-send-body invariant
// green.
//
// PINNED TRUTH (assert what it does, not what we'd guess): fail-once-then-succeed ⇒ the request recovers
// (200, nil err), forward returns attempts == 2, the upstream is hit exactly twice, and the SAME body is
// re-sent on the retry. NOTE/flag: forward RETURNS `attempts`, but both call sites currently DISCARD it
// (`resp, rb, _, err := p.forward(...)` at proxy.go:2307 and vision_dispatch.go:67) — the count is computed
// and correct but presently unconsumed. We pin the returned value's correctness regardless, so step (b)
// can't silently regress it.
func TestForward_SameProviderRetryThenSuccess_Characterization(t *testing.T) {
	var mu sync.Mutex
	var calls int
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		b, _ := io.ReadAll(rr.Body)
		mu.Lock()
		calls++
		n := calls
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 — retryable; first attempt FAILS
			return
		}
		w.WriteHeader(http.StatusOK) // attempt 2 succeeds
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"recovered"}}]}`)
	}))
	defer upstream.Close()

	p := newProxyWithFallback(t, upstream.URL, "", "")
	// Override the test default (fastRetry has MaxAttempts:1 = NO retry). A finite budget with 503 retryable,
	// zero delay ⇒ fast + deterministic same-provider recovery.
	p.retryConfig = retry.Config{MaxAttempts: 3, BaseDelay: 0, MaxDelay: 0, RetryableCodes: []int{429, 500, 502, 503, 504}}
	cfg := p.configForProvider("openai")

	body := `{"model":"gpt-4o","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))

	resp, respBody, attempts, err := p.forward(context.Background(), r, []byte(body), "gpt-4o", cfg)
	if err != nil {
		t.Fatalf("forward must RECOVER on the retry (nil err), got %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	gotCalls := calls
	gotBodies := append([]string(nil), bodies...)
	mu.Unlock()

	t.Logf("PINNED forward same-provider retry behavior:")
	t.Logf("  recovered          = %v (resp status %d, err nil)", err == nil, resp.StatusCode)
	t.Logf("  attempts returned  = %d  (fail-once-then-succeed)", attempts)
	t.Logf("  upstream call count = %d  (the retry really re-sent)", gotCalls)
	t.Logf("  bodies received    = %q  (identical payload re-sent on retry)", gotBodies)
	t.Logf("  NOTE: both forward callers discard `attempts` (`_`) today — pinned for correctness anyway")

	// 1. SAME-PROVIDER recovery: success without fallback.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("recovered response status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(respBody), "recovered") {
		t.Errorf("respBody = %q, want the attempt-2 success body", string(respBody))
	}
	// 2. ATTEMPT COUNT: forward returns attempts == the attempt number reached (2 for fail-once-then-succeed).
	if attempts != 2 {
		t.Errorf("attempts returned = %d, want 2 (retry.Do sets Attempts to the attempt reached)", attempts)
	}
	// 3. UPSTREAM CALL COUNT: the retry actually re-sent — exactly 2 POSTs, not 1, not 3.
	if gotCalls != 2 {
		t.Errorf("upstream call count = %d, want 2 (one failure + one success)", gotCalls)
	}
	// 4. RE-SENT BODY: the retry re-sends the IDENTICAL payload (forward rebuilds the request over the same
	//    body bytes each attempt — it must not drop or mutate it).
	if len(gotBodies) == 2 && gotBodies[0] != gotBodies[1] {
		t.Errorf("retry sent a DIFFERENT body: first=%q retry=%q", gotBodies[0], gotBodies[1])
	}
	for i, b := range gotBodies {
		if b != body {
			t.Errorf("upstream call %d body = %q, want the original %q", i+1, b, body)
		}
	}
}
