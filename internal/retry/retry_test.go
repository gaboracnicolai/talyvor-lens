package retry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedServer returns the given status codes in order, one per call.
// The last entry is repeated if more requests arrive than entries.
// Headers map applies to the response with the matching status (the
// first match wins).
func scriptedServer(t *testing.T, statuses []int, headers map[int]http.Header) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := atomic.AddInt32(&calls, 1) - 1
		idx := int(i)
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		status := statuses[idx]
		if h, ok := headers[status]; ok {
			for k, v := range h {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// fastConfig is a Config with tiny delays so retry tests finish in
// milliseconds rather than seconds.
func fastConfig() Config {
	return Config{
		MaxAttempts:    3,
		BaseDelay:      5 * time.Millisecond,
		MaxDelay:       50 * time.Millisecond,
		RetryableCodes: []int{429, 500, 502, 503, 504},
	}
}

func doRequest(srv *httptest.Server) func(context.Context) (*http.Response, error) {
	return func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
		if err != nil {
			return nil, err
		}
		return http.DefaultClient.Do(req)
	}
}

func TestDo_SuccessFirstAttemptNoRetry(t *testing.T) {
	srv, calls := scriptedServer(t, []int{http.StatusOK}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.LastError != nil {
		t.Fatalf("LastError = %v, want nil", result.LastError)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusOK {
		t.Fatalf("got status = %d, want 200", result.Response.StatusCode)
	}
	if result.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", result.Attempts)
	}
	if *calls != 1 {
		t.Errorf("server hit %d times, want 1", *calls)
	}
	if result.TotalDelay != 0 {
		t.Errorf("TotalDelay = %v, want 0 on first-attempt success", result.TotalDelay)
	}
}

func TestDo_RetryOn429ThenSuccess(t *testing.T) {
	srv, calls := scriptedServer(t, []int{http.StatusTooManyRequests, http.StatusOK}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.LastError != nil {
		t.Fatalf("LastError = %v, want nil", result.LastError)
	}
	if result.Response.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want 200", result.Response.StatusCode)
	}
	if result.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", result.Attempts)
	}
	if *calls != 2 {
		t.Errorf("server hit %d times, want 2", *calls)
	}
}

func TestDo_RetryOn503ThenSuccessOnThirdAttempt(t *testing.T) {
	srv, calls := scriptedServer(t, []int{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusOK,
	}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.LastError != nil {
		t.Fatalf("LastError = %v, want nil", result.LastError)
	}
	if result.Response.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want 200", result.Response.StatusCode)
	}
	if result.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", result.Attempts)
	}
	if *calls != 3 {
		t.Errorf("server hit %d times, want 3", *calls)
	}
}

func TestDo_ExhaustedRetriesReturnsLastResponse(t *testing.T) {
	srv, calls := scriptedServer(t, []int{http.StatusServiceUnavailable}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.Attempts != fastConfig().MaxAttempts {
		t.Errorf("Attempts = %d, want %d", result.Attempts, fastConfig().MaxAttempts)
	}
	if *calls != int32(fastConfig().MaxAttempts) {
		t.Errorf("server hit %d times, want %d", *calls, fastConfig().MaxAttempts)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("final response status = %d, want 503", result.Response.StatusCode)
	}
}

func TestDo_NonRetryable400(t *testing.T) {
	srv, calls := scriptedServer(t, []int{http.StatusBadRequest}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no retry on 400)", result.Attempts)
	}
	if *calls != 1 {
		t.Errorf("server hit %d times, want 1", *calls)
	}
	if result.Response.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", result.Response.StatusCode)
	}
}

func TestDo_NonRetryable404(t *testing.T) {
	srv, calls := scriptedServer(t, []int{http.StatusNotFound}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no retry on 404)", result.Attempts)
	}
	if *calls != 1 {
		t.Errorf("server hit %d times, want 1", *calls)
	}
}

func TestDo_RetryAfterHeaderRespectedOn429(t *testing.T) {
	headers := map[int]http.Header{
		http.StatusTooManyRequests: {"Retry-After": []string{"1"}},
	}
	srv, _ := scriptedServer(t, []int{http.StatusTooManyRequests, http.StatusOK}, headers)

	cfg := fastConfig()
	cfg.BaseDelay = 5 * time.Millisecond // would yield ~5ms backoff
	cfg.MaxDelay = 5 * time.Millisecond

	start := time.Now()
	result := Do(context.Background(), cfg, doRequest(srv))
	elapsed := time.Since(start)

	if result.LastError != nil {
		t.Fatalf("LastError = %v, want nil", result.LastError)
	}
	if result.Response.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", result.Response.StatusCode)
	}
	// Retry-After: 1 must beat our 5ms backoff and have us wait ~1s.
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥ 900ms (Retry-After should override config)", elapsed)
	}
}

func TestDo_ContextCancellationStopsRetry(t *testing.T) {
	srv, _ := scriptedServer(t, []int{http.StatusServiceUnavailable}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	cfg := Config{
		MaxAttempts:    10,
		BaseDelay:      1 * time.Second,
		MaxDelay:       5 * time.Second,
		RetryableCodes: []int{503},
	}

	start := time.Now()
	result := Do(ctx, cfg, doRequest(srv))
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want < 500ms (cancel should short-circuit sleep)", elapsed)
	}
	if !errors.Is(result.LastError, context.Canceled) {
		t.Errorf("LastError = %v, want context.Canceled", result.LastError)
	}
}

func TestDo_TotalDelayGreaterThanZeroOnRetry(t *testing.T) {
	srv, _ := scriptedServer(t, []int{http.StatusServiceUnavailable, http.StatusOK}, nil)

	result := Do(context.Background(), fastConfig(), doRequest(srv))
	if result.LastError != nil {
		t.Fatalf("LastError = %v", result.LastError)
	}
	if result.TotalDelay <= 0 {
		t.Errorf("TotalDelay = %v, want > 0", result.TotalDelay)
	}
}

func TestDo_AttemptsCountIsCorrect(t *testing.T) {
	cases := []struct {
		name     string
		statuses []int
		want     int
	}{
		{"success first try", []int{200}, 1},
		{"retry once", []int{503, 200}, 2},
		{"retry twice", []int{503, 503, 200}, 3},
		{"exhausted", []int{503}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := scriptedServer(t, tc.statuses, nil)
			result := Do(context.Background(), fastConfig(), doRequest(srv))
			if result.Attempts != tc.want {
				t.Errorf("Attempts = %d, want %d", result.Attempts, tc.want)
			}
		})
	}
}

// Sanity check that Retry-After malformed values don't crash the loop.
func TestDo_MalformedRetryAfterFallsBackToBackoff(t *testing.T) {
	headers := map[int]http.Header{
		http.StatusTooManyRequests: {"Retry-After": []string{"not-a-number"}},
	}
	srv, _ := scriptedServer(t, []int{http.StatusTooManyRequests, http.StatusOK}, headers)

	cfg := fastConfig()
	start := time.Now()
	result := Do(context.Background(), cfg, doRequest(srv))
	elapsed := time.Since(start)
	if result.LastError != nil {
		t.Fatal(result.LastError)
	}
	// Should not have waited a full second; backoff is < 100ms.
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want < 500ms (bad Retry-After should fall back to backoff)", elapsed)
	}
	_ = strconv.Itoa // keep strconv used
}
