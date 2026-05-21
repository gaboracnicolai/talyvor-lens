package ab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/quality"
)

const (
	openAIChatURL       = "https://api.openai.com/v1/chat/completions"
	anthropicMessageURL = "https://api.anthropic.com/v1/messages"
	// qualityCloseRatio is the fraction of A's score that B must match
	// (or beat) for B to qualify as a winner. 0.95 means "within 5%".
	qualityCloseRatio = 0.95
)

// pgxDB is the subset of *pgxpool.Pool that Tester needs. Tests pass nil
// to skip persistence — the in-memory map is the source of truth either way.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Tester struct {
	pool   pgxDB
	scorer *quality.Scorer
	mu     sync.RWMutex
	tests  map[string]*ABTest

	// Upstream URLs are unexported and defaulted so tests can swap them
	// for an httptest server without leaking config to callers.
	openAIURL    string
	anthropicURL string
}

type ABTest struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	ModelA     string    `json:"model_a"`
	ModelB     string    `json:"model_b"`
	SamplePct  float64   `json:"sample_pct"`
	MinSamples int       `json:"min_samples"`
	Results    ABResults `json:"results"`
	Active     bool      `json:"active"`
	CreatedAt  time.Time `json:"created_at"`
}

type ABResults struct {
	ModelA     ModelStats `json:"model_a"`
	ModelB     ModelStats `json:"model_b"`
	Winner     string     `json:"winner"`
	Confidence float64    `json:"confidence"`
}

type ModelStats struct {
	Samples      int     `json:"samples"`
	AvgScore     float64 `json:"avg_score"`
	AvgCostUSD   float64 `json:"avg_cost_usd"`
	AvgLatencyMs int64   `json:"avg_latency_ms"`
}

type ShadowResult struct {
	Model     string  `json:"model"`
	Response  string  `json:"response"`
	Score     float64 `json:"score"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMs int64   `json:"latency_ms"`
}

func New(pool *pgxpool.Pool, scorer *quality.Scorer) *Tester {
	// Avoid the typed-nil interface trap: a (*pgxpool.Pool)(nil) stored
	// in pgxDB compares != nil but panics on call.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Tester{
		pool:         db,
		scorer:       scorer,
		tests:        make(map[string]*ABTest),
		openAIURL:    openAIChatURL,
		anthropicURL: anthropicMessageURL,
	}
}

const insertTestSQL = `INSERT INTO ab_tests (id, provider, model_a, model_b, sample_pct, min_samples, active)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET
  provider    = EXCLUDED.provider,
  model_a     = EXCLUDED.model_a,
  model_b     = EXCLUDED.model_b,
  sample_pct  = EXCLUDED.sample_pct,
  min_samples = EXCLUDED.min_samples,
  active      = EXCLUDED.active`

func (t *Tester) RegisterTest(test ABTest) error {
	if test.ID == "" {
		return errors.New("ab: test ID required")
	}
	if test.ModelA == test.ModelB {
		return errors.New("ab: ModelA and ModelB must differ")
	}
	if test.SamplePct <= 0 || test.SamplePct > 1 {
		return fmt.Errorf("ab: SamplePct must be in (0, 1]; got %v", test.SamplePct)
	}
	if test.MinSamples < 10 {
		return fmt.Errorf("ab: MinSamples must be >= 10; got %d", test.MinSamples)
	}
	if test.Provider == "" {
		return errors.New("ab: Provider required")
	}
	if test.CreatedAt.IsZero() {
		test.CreatedAt = time.Now().UTC()
	}

	t.mu.Lock()
	stored := test
	t.tests[test.ID] = &stored
	t.mu.Unlock()

	if t.pool != nil {
		if _, err := t.pool.Exec(context.Background(), insertTestSQL,
			stored.ID, stored.Provider, stored.ModelA, stored.ModelB,
			stored.SamplePct, stored.MinSamples, stored.Active,
		); err != nil {
			slog.Warn("ab: INSERT ab_tests failed", slog.String("err", err.Error()))
		}
	}
	return nil
}

func (t *Tester) ShouldShadow(testID string) bool {
	t.mu.RLock()
	test, ok := t.tests[testID]
	pct := 0.0
	active := false
	if ok {
		pct = test.SamplePct
		active = test.Active
	}
	t.mu.RUnlock()
	if !ok || !active {
		return false
	}
	return rand.Float64() < pct
}

// ActiveTestsFor returns the registered active tests that target the given
// provider + model pair. Used by the proxy to decide which shadow probes
// to fire after a successful main request.
func (t *Tester) ActiveTestsFor(provider, model string) []ABTest {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []ABTest
	for _, test := range t.tests {
		if test.Active && test.Provider == provider && test.ModelA == model {
			out = append(out, *test)
		}
	}
	return out
}

// substituteModel deep-copies the request body and replaces the "model"
// field with the challenger. Other top-level fields (system, messages,
// temperature, tools, ...) are preserved.
func substituteModel(body []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("ab: parse body: %w", err)
	}
	m["model"] = model
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("ab: marshal body: %w", err)
	}
	return out, nil
}

func providerForModel(model string) string {
	if strings.HasPrefix(model, "claude-") {
		return "anthropic"
	}
	return "openai"
}

func (t *Tester) upstreamFor(provider string) string {
	if provider == "anthropic" {
		return t.anthropicURL
	}
	return t.openAIURL
}

func applyAuthHeaders(req *http.Request, provider, apiKey string) {
	if provider == "anthropic" {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
}

func (t *Tester) RunShadow(
	ctx context.Context,
	testID string,
	prompt string,
	body []byte,
	httpClient *http.Client,
	apiKey string,
) (*ShadowResult, error) {
	t.mu.RLock()
	test, ok := t.tests[testID]
	var modelB, provider string
	if ok {
		modelB = test.ModelB
		provider = test.Provider
	}
	t.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("ab: unknown test %q", testID)
	}

	mutated, err := substituteModel(body, modelB)
	if err != nil {
		return nil, err
	}

	// Derive provider from model name as a defense in case the test was
	// registered with a mismatched provider/model pair.
	if mp := providerForModel(modelB); provider == "" {
		provider = mp
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.upstreamFor(provider), bytes.NewReader(mutated))
	if err != nil {
		return nil, fmt.Errorf("ab: build shadow request: %w", err)
	}
	applyAuthHeaders(req, provider, apiKey)

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ab: shadow upstream: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ab: read shadow response: %w", err)
	}
	latency := time.Since(start).Milliseconds()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ab: shadow upstream status %d", resp.StatusCode)
	}

	respText := string(respBody)
	var score float64
	if t.scorer != nil {
		score = t.scorer.ScoreResponse(ctx, prompt, respText, provider, modelB).Score
	}

	// Cost is computed on len/4 token estimates — same convention used by
	// the rest of the pipeline (router complexity, recordTokenEvent, etc.).
	cost := alerts.CostUSD(modelB, len(prompt)/4, len(respText)/4)

	return &ShadowResult{
		Model:     modelB,
		Response:  respText,
		Score:     score,
		CostUSD:   cost,
		LatencyMs: latency,
	}, nil
}

func (t *Tester) RecordResult(_ context.Context, testID, model string, result ShadowResult) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	test, ok := t.tests[testID]
	if !ok {
		return fmt.Errorf("ab: unknown test %q", testID)
	}

	switch model {
	case test.ModelA:
		updateStats(&test.Results.ModelA, result)
	case test.ModelB:
		updateStats(&test.Results.ModelB, result)
	default:
		return fmt.Errorf("ab: model %q is neither ModelA nor ModelB for test %q", model, testID)
	}

	a, b := test.Results.ModelA, test.Results.ModelB
	test.Results.Winner = computeWinner(a, b, test.MinSamples)
	test.Results.Confidence = computeConfidence(a, b, test.MinSamples)
	return nil
}

// updateStats folds a new sample into the running averages. We store the
// running mean directly (rather than sums) since the numbers are bounded
// and overflow is not a concern at expected sample sizes.
func updateStats(s *ModelStats, r ShadowResult) {
	prev := s.Samples
	s.Samples = prev + 1
	n := float64(s.Samples)
	s.AvgScore = (s.AvgScore*float64(prev) + r.Score) / n
	s.AvgCostUSD = (s.AvgCostUSD*float64(prev) + r.CostUSD) / n
	s.AvgLatencyMs = int64((float64(s.AvgLatencyMs)*float64(prev) + float64(r.LatencyMs)) / n)
}

func computeWinner(a, b ModelStats, minSamples int) string {
	if a.Samples < minSamples || b.Samples < minSamples {
		return ""
	}
	if b.AvgScore < a.AvgScore*qualityCloseRatio {
		return "A"
	}
	// B's quality is within the close-ratio band of A.
	if b.AvgCostUSD < a.AvgCostUSD {
		return "B"
	}
	return ""
}

func computeConfidence(a, b ModelStats, minSamples int) float64 {
	if minSamples <= 0 {
		return 0
	}
	smaller := a.Samples
	if b.Samples < smaller {
		smaller = b.Samples
	}
	c := float64(smaller) / float64(minSamples)
	if c > 1 {
		c = 1
	}
	return c
}

func (t *Tester) GetResults(testID string) (*ABTest, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	test, ok := t.tests[testID]
	if !ok {
		return nil, false
	}
	// Return a copy so the caller can't mutate our state.
	copy := *test
	return &copy, true
}
