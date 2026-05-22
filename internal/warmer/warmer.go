package warmer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/learner"
)

const (
	openAIChatURL       = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL = "https://api.anthropic.com/v1/messages"
	warmBatchSize       = 10
	warmTimeout         = 30 * time.Second
)

// pgxDB is the subset of *pgxpool.Pool that the warmer needs. Tests use
// pgxmock; production passes a real pool.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Warmer struct {
	pool         pgxDB
	learner      *learner.Learner // kept per spec; not used in current logic
	exactCache   *cache.ExactCache
	httpClient   *http.Client
	openAIKey    string
	anthropicKey string
	mu           sync.RWMutex
	warming      map[string]bool

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server.
	openAIURL    string
	anthropicURL string
}

type WarmJob struct {
	PromptHash string
	Provider   string
	Model      string
	HitCount   int
	LastPrompt string
}

type WarmResult struct {
	PromptHash string
	Success    bool
	Cached     bool
	Error      error
}

func New(
	pool *pgxpool.Pool,
	lrn *learner.Learner,
	exactCache *cache.ExactCache,
	openAIKey, anthropicKey string,
) *Warmer {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	w := newWarmer(db, exactCache, openAIKey, anthropicKey)
	w.learner = lrn
	return w
}

func newWarmer(pool pgxDB, exactCache *cache.ExactCache, openAIKey, anthropicKey string) *Warmer {
	return &Warmer{
		pool:         pool,
		exactCache:   exactCache,
		httpClient:   &http.Client{Timeout: warmTimeout},
		openAIKey:    openAIKey,
		anthropicKey: anthropicKey,
		warming:      make(map[string]bool),
		openAIURL:    openAIChatURL,
		anthropicURL: anthropicMessageURL,
	}
}

const candidatesSQL = `SELECT pe.prompt_hash, pe.provider, pe.model, pe.hit_count, te.prompt_text
FROM prompt_embeddings pe
JOIN token_events te ON te.prompt_hash = pe.prompt_hash
WHERE pe.hit_count >= 5
  AND pe.updated_at < NOW() - INTERVAL '1 hour'
GROUP BY pe.prompt_hash, pe.provider, pe.model, pe.hit_count, te.prompt_text
ORDER BY pe.hit_count DESC
LIMIT $1`

// GetWarmCandidates returns up to `limit` patterns that are popular
// (hit_count >= 5) and haven't been touched in the last hour. The join
// with token_events pulls the actual prompt text we need to re-fire
// against the upstream LLM.
func (w *Warmer) GetWarmCandidates(ctx context.Context, limit int) ([]WarmJob, error) {
	if w.pool == nil {
		return nil, nil
	}
	rows, err := w.pool.Query(ctx, candidatesSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("warmer: query candidates: %w", err)
	}
	defer rows.Close()

	var out []WarmJob
	for rows.Next() {
		var j WarmJob
		if err := rows.Scan(&j.PromptHash, &j.Provider, &j.Model, &j.HitCount, &j.LastPrompt); err != nil {
			return nil, fmt.Errorf("warmer: scan candidate: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// WarmOne fetches a fresh response for a single popular pattern and
// stores it in the exact cache. Dedups concurrent attempts: if another
// goroutine is already warming the same hash we no-op with Cached=true.
// Always returns — never panics — so a single-pattern failure can't
// take the whole cycle down.
func (w *Warmer) WarmOne(ctx context.Context, job WarmJob) WarmResult {
	res := WarmResult{PromptHash: job.PromptHash}

	// Dedup: in-flight check.
	w.mu.Lock()
	if w.warming[job.PromptHash] {
		w.mu.Unlock()
		res.Cached = true
		return res
	}
	w.warming[job.PromptHash] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.warming, job.PromptHash)
		w.mu.Unlock()
	}()

	// Skip if cache already has it.
	if w.exactCache != nil {
		if cached, _ := w.exactCache.Get(ctx, job.Provider, job.Model, job.LastPrompt); cached != nil {
			res.Cached = true
			return res
		}
	}

	// Each warm gets its own bounded context — we don't inherit the
	// parent ctx's deadline (it may already be near-expired from a long
	// previous warm) but we still respect the parent's cancellation.
	warmCtx, cancel := context.WithTimeout(ctx, warmTimeout)
	defer cancel()

	body, err := w.fetchFresh(warmCtx, job)
	if err != nil {
		res.Error = err
		return res
	}

	if w.exactCache != nil {
		if err := w.exactCache.Set(ctx, job.Provider, job.Model, job.LastPrompt, body); err != nil {
			slog.Warn("warmer: cache store failed",
				slog.String("source", "cache_warmer"),
				slog.String("prompt_hash", job.PromptHash),
				slog.String("err", err.Error()),
			)
		}
	}
	res.Success = true
	return res
}

// fetchFresh assembles a minimal request envelope for the target
// provider and dispatches it. The body we send is intentionally
// minimal — model + a single user message — to maximise cache fidelity
// with the proxy's rebuilt-body shape.
func (w *Warmer) fetchFresh(ctx context.Context, job WarmJob) ([]byte, error) {
	var (
		upstreamURL string
		body        []byte
		apiKey      string
		applyAuth   func(*http.Request)
	)

	switch job.Provider {
	case "anthropic":
		upstreamURL = w.anthropicURL
		apiKey = w.anthropicKey
		payload, err := json.Marshal(map[string]any{
			"model": job.Model,
			"messages": []map[string]any{{
				"role":    "user",
				"content": job.LastPrompt,
			}},
		})
		if err != nil {
			return nil, err
		}
		body = payload
		applyAuth = func(req *http.Request) {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	default:
		upstreamURL = w.openAIURL
		apiKey = w.openAIKey
		payload, err := json.Marshal(map[string]any{
			"model": job.Model,
			"messages": []map[string]any{{
				"role":    "user",
				"content": job.LastPrompt,
			}},
		})
		if err != nil {
			return nil, err
		}
		body = payload
		applyAuth = func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("warmer: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("warmer: upstream: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("warmer: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("warmer: upstream status %d: %s", resp.StatusCode, snippet(respBody))
	}
	return respBody, nil
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// RunCycle fans out warming across all current candidates in parallel.
// Logs an aggregate summary at the end so dashboards see a single
// log line per cycle rather than per warm.
func (w *Warmer) RunCycle(ctx context.Context) []WarmResult {
	jobs, err := w.GetWarmCandidates(ctx, warmBatchSize)
	if err != nil {
		slog.Warn("warmer: GetWarmCandidates failed",
			slog.String("source", "cache_warmer"),
			slog.String("err", err.Error()),
		)
		return nil
	}
	if len(jobs) == 0 {
		return nil
	}

	results := make([]WarmResult, len(jobs))
	var wg sync.WaitGroup
	wg.Add(len(jobs))
	for i, j := range jobs {
		i, j := i, j
		go func() {
			defer wg.Done()
			results[i] = w.WarmOne(ctx, j)
		}()
	}
	wg.Wait()

	var warmed, skipped, failed int
	for _, r := range results {
		switch {
		case r.Success:
			warmed++
		case r.Cached:
			skipped++
		default:
			failed++
		}
	}
	slog.Info("warmer: cycle complete",
		slog.String("source", "cache_warmer"),
		slog.Int("warmed", warmed),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed),
	)
	return results
}

// Start spawns the background loop. The first cycle fires after one
// interval (not immediately) so a freshly-started process doesn't slam
// the LLM during its first uptime second.
func (w *Warmer) Start(ctx context.Context, interval time.Duration) {
	w.runLoop(ctx, interval)
}

func (w *Warmer) runLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RunCycle(ctx)
		}
	}
}
