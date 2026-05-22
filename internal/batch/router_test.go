package batch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newTestRouter(t *testing.T, anthropicURL string) (*BatchRouter, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	r := newBatchRouter(pool, "anthropic-key")
	if anthropicURL != "" {
		r.anthropicURL = anthropicURL
	}
	return r, pool
}

func TestIsEligible_AnthropicBatchHeader(t *testing.T) {
	r, _ := newTestRouter(t, "")
	body := []byte(`{"model":"claude-3-opus-20240229","batch_eligible":true}`)

	got := r.IsEligible(body, "ws-1")
	if !got.Eligible {
		t.Errorf("Eligible = false; reason=%q", got.Reason)
	}
}

func TestIsEligible_FalseForOpenAI(t *testing.T) {
	r, _ := newTestRouter(t, "")
	body := []byte(`{"model":"gpt-4o","batch_eligible":true}`)

	got := r.IsEligible(body, "ws-1")
	if got.Eligible {
		t.Errorf("Eligible = true for openai; want false")
	}
}

func TestIsEligible_FalseWhenStreaming(t *testing.T) {
	r, _ := newTestRouter(t, "")
	body := []byte(`{"model":"claude-3-opus","batch_eligible":true,"stream":true}`)

	got := r.IsEligible(body, "ws-1")
	if got.Eligible {
		t.Errorf("Eligible = true for streaming request; want false")
	}
}

func TestIsEligible_FalseWithoutBatchHeader(t *testing.T) {
	r, _ := newTestRouter(t, "")
	// claude model + workspace, but no batch_eligible flag set.
	body := []byte(`{"model":"claude-3-opus"}`)

	got := r.IsEligible(body, "ws-1")
	if got.Eligible {
		t.Errorf("Eligible = true without batch_eligible flag; want false")
	}
}

func TestSubmit_CreatesBatchJobAndStoresInMemory(t *testing.T) {
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		if got := r.Header.Get("x-api-key"); got != "anthropic-key" {
			t.Errorf("x-api-key = %q, want anthropic-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want 2023-06-01", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"batch_abc123","processing_status":"in_progress"}`)
	}))
	t.Cleanup(srv.Close)

	r, pool := newTestRouter(t, srv.URL)
	pool.ExpectExec(`INSERT INTO batch_jobs`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	body := []byte(`{"model":"claude-3-opus-20240229","messages":[{"role":"user","content":"hi"}],"batch_eligible":true}`)
	job, err := r.Submit(context.Background(), "ws-1", "claude-3-opus-20240229", "hi", body)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if job == nil {
		t.Fatal("Submit returned nil job")
	}
	if !strings.HasSuffix(sawPath, "/v1/messages/batches") {
		t.Errorf("upstream path = %q, want /v1/messages/batches", sawPath)
	}
	if job.ID != "batch_abc123" {
		t.Errorf("job.ID = %q, want batch_abc123", job.ID)
	}
	if job.Status != BatchPending {
		t.Errorf("job.Status = %q, want %q", job.Status, BatchPending)
	}

	// In-memory pending map should have it under requestID.
	r.mu.RLock()
	_, ok := r.pending[job.RequestID]
	r.mu.RUnlock()
	if !ok {
		t.Error("Submit did not store the job in the pending map")
	}
}

func TestPoll_ReturnsProcessingWhileInProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"batch_xy","processing_status":"in_progress"}`)
	}))
	t.Cleanup(srv.Close)

	r, _ := newTestRouter(t, srv.URL)

	// Seed the job in memory so Poll can look up the linked request.
	requestID := "req-1"
	job := &BatchJob{
		ID:        "batch_xy",
		RequestID: requestID,
		Provider:  "anthropic",
		Model:     "claude-3-opus",
		Status:    BatchPending,
		CreatedAt: time.Now(),
	}
	r.mu.Lock()
	r.pending[requestID] = job
	r.mu.Unlock()

	got, err := r.Poll(context.Background(), "batch_xy")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got == nil || got.Status != BatchProcessing {
		t.Errorf("got status = %q, want %q", got.Status, BatchProcessing)
	}
}

func TestPoll_CompleteExtractsResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/results"):
			// JSONL line per request.
			_, _ = io.WriteString(w, `{"custom_id":"req-2","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"batch result"}]}}}`+"\n")
		default:
			_, _ = io.WriteString(w, `{"id":"batch_done","processing_status":"ended"}`)
		}
	}))
	t.Cleanup(srv.Close)

	r, _ := newTestRouter(t, srv.URL)

	requestID := "req-2"
	r.mu.Lock()
	r.pending[requestID] = &BatchJob{
		ID:        "batch_done",
		RequestID: requestID,
		Provider:  "anthropic",
		Model:     "claude-3-opus-20240229",
		Prompt:    "hi",
		Status:    BatchProcessing,
		CreatedAt: time.Now(),
	}
	r.mu.Unlock()

	got, err := r.Poll(context.Background(), "batch_done")
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.Status != BatchComplete {
		t.Fatalf("Status = %q, want %q", got.Status, BatchComplete)
	}
	if !strings.Contains(string(got.Response), "batch result") {
		t.Errorf("Response missing batch text: %s", got.Response)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt should be set on complete")
	}
}

func TestStartPoller_StopsOnContextCancel(t *testing.T) {
	r, _ := newTestRouter(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Long interval so only the cancel exits the loop.
		r.pollLoop(ctx, time.Hour)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop did not exit within 2s of cancel")
	}
	wg.Wait()

	// Silence unused json import if a future edit drops it.
	_ = json.RawMessage{}
}
