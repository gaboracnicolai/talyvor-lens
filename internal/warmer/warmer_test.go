package warmer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
)

func setupWarmer(t *testing.T, openAIURL string) (*Warmer, *cache.ExactCache, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	exact := cache.NewExactCache(rc, time.Minute)

	w := newWarmer(pool, exact, "openai-key", "anthropic-key")
	if openAIURL != "" {
		w.openAIURL = openAIURL
	}
	return w, exact, pool
}

func TestGetWarmCandidates_ReturnsJobsFromDB(t *testing.T) {
	w, _, pool := setupWarmer(t, "")

	pool.ExpectQuery(`FROM prompt_embeddings`).
		WithArgs(10).
		WillReturnRows(
			pgxmock.NewRows([]string{"prompt_hash", "provider", "model", "hit_count", "prompt_text"}).
				AddRow("hash-1", "openai", "gpt-4o", int(12), "What is AI?").
				AddRow("hash-2", "openai", "gpt-4o-mini", int(8), "Hello world"),
		)

	jobs, err := w.GetWarmCandidates(context.Background(), 10)
	if err != nil {
		t.Fatalf("GetWarmCandidates: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	if jobs[0].PromptHash != "hash-1" || jobs[0].LastPrompt != "What is AI?" {
		t.Errorf("jobs[0] = %+v, want hash-1/'What is AI?'", jobs[0])
	}
	if jobs[1].Model != "gpt-4o-mini" {
		t.Errorf("jobs[1].Model = %q, want gpt-4o-mini", jobs[1].Model)
	}
}

func TestWarmOne_SkipsWhenAlreadyWarming(t *testing.T) {
	w, _, _ := setupWarmer(t, "")

	// Pre-mark the hash as in-flight to simulate concurrent warming.
	w.mu.Lock()
	w.warming["hash-x"] = true
	w.mu.Unlock()

	res := w.WarmOne(context.Background(), WarmJob{
		PromptHash: "hash-x", Provider: "openai", Model: "gpt-4o", LastPrompt: "x",
	})
	if !res.Cached {
		t.Errorf("Cached = false, want true (dedup should skip)")
	}
	if res.Success {
		t.Errorf("Success = true, want false on dedup-skip")
	}
}

func TestWarmOne_CachedTrueWhenAlreadyInCache(t *testing.T) {
	w, exact, _ := setupWarmer(t, "")

	if err := exact.Set(context.Background(), "openai", "gpt-4o", "hi", []byte(`{"choices":[]}`)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	res := w.WarmOne(context.Background(), WarmJob{
		PromptHash: "h", Provider: "openai", Model: "gpt-4o", LastPrompt: "hi",
	})
	if !res.Cached {
		t.Errorf("Cached = false, want true (cache already has it)")
	}
	if res.Success {
		t.Errorf("Success = true, want false when no fresh work was done")
	}
}

func TestWarmOne_CallsLLMAndStoresOnMiss(t *testing.T) {
	var sawModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header forwarded.
		if got := r.Header.Get("Authorization"); got != "Bearer openai-key" {
			t.Errorf("Authorization = %q, want Bearer openai-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"warmed response"}}]}`)
		sawModel = r.URL.Path
	}))
	t.Cleanup(srv.Close)

	wr, exact, _ := setupWarmer(t, srv.URL)

	res := wr.WarmOne(context.Background(), WarmJob{
		PromptHash: "h-miss", Provider: "openai", Model: "gpt-4o", LastPrompt: "warm me",
	})
	if !res.Success {
		t.Fatalf("Success = false; err=%v", res.Error)
	}

	cached, err := exact.Get(context.Background(), "openai", "gpt-4o", "warm me")
	if err != nil {
		t.Fatalf("exact.Get: %v", err)
	}
	if cached == nil {
		t.Fatal("cache not populated after WarmOne")
	}
	if !strings.Contains(string(cached), "warmed response") {
		t.Errorf("cached body missing upstream content: %s", cached)
	}
	if sawModel == "" {
		t.Error("upstream was not hit")
	}
}

func TestWarmOne_HandlesLLMErrorGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	wr, exact, _ := setupWarmer(t, srv.URL)

	res := wr.WarmOne(context.Background(), WarmJob{
		PromptHash: "h-err", Provider: "openai", Model: "gpt-4o", LastPrompt: "fails",
	})
	if res.Success {
		t.Errorf("Success = true, want false on LLM error")
	}
	if res.Error == nil {
		t.Errorf("Error = nil, want non-nil")
	}

	// Cache must NOT be populated on a failed warm.
	if cached, _ := exact.Get(context.Background(), "openai", "gpt-4o", "fails"); cached != nil {
		t.Errorf("cache populated despite warm failure: %s", cached)
	}
}

func TestRunCycle_ProcessesMultipleCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)

	wr, _, pool := setupWarmer(t, srv.URL)

	pool.ExpectQuery(`FROM prompt_embeddings`).
		WithArgs(warmBatchSize).
		WillReturnRows(
			pgxmock.NewRows([]string{"prompt_hash", "provider", "model", "hit_count", "prompt_text"}).
				AddRow("hash-a", "openai", "gpt-4o", int(10), "prompt-a").
				AddRow("hash-b", "openai", "gpt-4o", int(7), "prompt-b"),
		)

	results := wr.RunCycle(context.Background())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Errorf("result %s failed: %v", r.PromptHash, r.Error)
		}
	}
}

func TestStart_StopsCleanlyOnContextCancellation(t *testing.T) {
	w, _, _ := setupWarmer(t, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// Hour-long interval — only the cancel should make this return.
		w.runLoop(ctx, time.Hour)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit within 2s of context cancel")
	}
}
