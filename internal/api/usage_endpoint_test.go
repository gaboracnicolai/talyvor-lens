package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
)

// /v1/api/usage — the per-model usage + cache-hit-rate read the trial's /spend and Overview
// screens need. Per-model {requests, in/out tokens, cost, cache_hits} PLUS the workspace hit rate,
// both over token_events, workspace-scoped from the authenticated key. The hit rate is measured
// from serve_source (migration 0100) — the ONLY signal that expresses it; the legacy `cached`
// boolean is never written true, so handleSpendSummary/handleCacheStats read a dead 0.

const usageSchema = "lens_usage_endpoint_test"

func usageHarness(t *testing.T) *Server {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG usage-endpoint test")
	}
	// PRIVATE SCHEMA (search_path) so this file's token_events never collides with the PUBLIC
	// token_events other api tests (spend_by_request) create in the shared LENS_TEST_DATABASE_URL DB.
	// A shared CREATE-TABLE-IF-NOT-EXISTS race would otherwise let whichever harness ran first pin the
	// column set. Same isolation the PoVI node harness uses.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = usageSchema
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// A minimal token_events shaped like the columns this endpoint reads, carrying serve_source
	// (0100). `cached` is present-but-never-set-true on purpose: proving the rate reads serve_source,
	// not the dead legacy boolean.
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS ` + usageSchema + ` CASCADE`,
		`CREATE SCHEMA ` + usageSchema,
		`CREATE TABLE token_events (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0, cost_usd FLOAT NOT NULL DEFAULT 0,
			cached BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ DEFAULT NOW(),
			serve_source TEXT NOT NULL DEFAULT 'upstream')`,
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return newServer(serverDeps{pool: pool})
}

// seedUsage inserts one token_events row with an explicit model + serve_source (+ a cached flag,
// which the endpoint must ignore).
func seedUsage(t *testing.T, s *Server, ws, model string, inTok, outTok int, cost float64, serveSource string, cached bool) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO token_events (workspace_id, model, input_tokens, output_tokens, cost_usd, cached, serve_source, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, NOW())`, ws, model, inTok, outTok, cost, cached, serveSource); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

type usageResp struct {
	PeriodDays int `json:"period_days"`
	Models     []struct {
		Model        string  `json:"model"`
		Requests     int64   `json:"requests"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		CostUSD      float64 `json:"cost_usd"`
		CacheHits    int64   `json:"cache_hits"`
	} `json:"models"`
	Cache struct {
		TotalRequests int64            `json:"total_requests"`
		CacheHits     int64            `json:"cache_hits"`
		Misses        int64            `json:"misses"`
		HitRate       float64          `json:"hit_rate"`
		BySource      map[string]int64 `json:"by_source"`
	} `json:"cache"`
}

// callUsage invokes the endpoint as `ws` (non-admin ⇒ scoped to ws).
func callUsage(t *testing.T, s *Server, ws, query string) usageResp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/api/usage?"+query, nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws}))
	rec := httptest.NewRecorder()
	s.handleUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp usageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	return resp
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// (1) THE CORE NUMBERS: per-model usage rolls up correctly, and the cache hit rate is measured
// from serve_source — hits vs the whole recorded denominator.
func TestUsage_PerModelAndHitRate(t *testing.T) {
	s := usageHarness(t)
	seedUsage(t, s, "wsA", "gpt-4o", 100, 50, 0.10, "upstream", false)
	seedUsage(t, s, "wsA", "gpt-4o", 100, 50, 0.10, "upstream", false)
	seedUsage(t, s, "wsA", "gpt-4o", 100, 50, 0.00, "cache_hit_exact", false)
	seedUsage(t, s, "wsA", "gpt-4o-mini", 20, 10, 0.01, "upstream", false)

	r := callUsage(t, s, "wsA", "days=1")

	// Per model — ordered by cost DESC → gpt-4o first.
	if len(r.Models) != 2 {
		t.Fatalf("want 2 models, got %d (%+v)", len(r.Models), r.Models)
	}
	m := r.Models[0]
	if m.Model != "gpt-4o" || m.Requests != 3 || m.InputTokens != 300 || m.OutputTokens != 150 || !approx(m.CostUSD, 0.20) || m.CacheHits != 1 {
		t.Fatalf("gpt-4o row wrong: %+v", m)
	}
	if r.Models[1].Model != "gpt-4o-mini" || r.Models[1].Requests != 1 || r.Models[1].CacheHits != 0 {
		t.Fatalf("gpt-4o-mini row wrong: %+v", r.Models[1])
	}

	// Cache rollup — hits from serve_source, over the whole denominator.
	c := r.Cache
	if c.TotalRequests != 4 || c.CacheHits != 1 || c.Misses != 3 || !approx(c.HitRate, 0.25) {
		t.Fatalf("cache rollup wrong: %+v", c)
	}
	if c.BySource["upstream"] != 3 || c.BySource["cache_hit_exact"] != 1 {
		t.Fatalf("by_source wrong: %+v", c.BySource)
	}
}

// (2) THE HIT RATE IS serve_source, NOT the dead `cached` boolean. A cache serve tagged in
// serve_source but with cached=false still counts as a hit; an upstream row with cached=true
// (a value nothing writes today, seeded here to prove the point) is NOT a hit.
func TestUsage_HitRateFromServeSource_NotCachedBool(t *testing.T) {
	s := usageHarness(t)
	seedUsage(t, s, "wsS", "m", 1, 1, 0, "cache_hit_semantic", false) // hit via serve_source, cached=false
	seedUsage(t, s, "wsS", "m", 1, 1, 0.05, "upstream", true)         // NOT a hit, despite cached=true

	r := callUsage(t, s, "wsS", "days=1")
	if r.Cache.CacheHits != 1 || r.Cache.TotalRequests != 2 || !approx(r.Cache.HitRate, 0.5) {
		t.Fatalf("rate must come from serve_source, not cached bool: %+v", r.Cache)
	}
	if r.Cache.BySource["cache_hit_semantic"] != 1 || r.Cache.BySource["upstream"] != 1 {
		t.Fatalf("by_source wrong: %+v", r.Cache.BySource)
	}
}

// (3) TENANCY: scoped to the authenticated workspace; another workspace's rows never appear.
func TestUsage_TenancyScoped(t *testing.T) {
	s := usageHarness(t)
	seedUsage(t, s, "wsA", "gpt-4o", 100, 50, 0.10, "upstream", false)
	seedUsage(t, s, "wsB", "gpt-4o", 100, 50, 0.10, "cache_hit_exact", false)

	r := callUsage(t, s, "wsA", "days=1")
	if r.Cache.TotalRequests != 1 || r.Cache.CacheHits != 0 {
		t.Fatalf("TENANCY LEAK: wsA must see only its 1 upstream row: %+v", r.Cache)
	}
	if len(r.Models) != 1 || r.Models[0].Requests != 1 {
		t.Fatalf("wsA must see exactly its own model rows: %+v", r.Models)
	}
}

// (4) EMPTY WINDOW: no rows ⇒ hit_rate 0, no divide-by-zero, empty models.
func TestUsage_EmptyWindow_NoDivByZero(t *testing.T) {
	s := usageHarness(t)
	r := callUsage(t, s, "wsEmpty", "days=1")
	if r.Cache.TotalRequests != 0 || r.Cache.HitRate != 0 || r.Cache.Misses != 0 {
		t.Fatalf("empty window must be all-zero, got %+v", r.Cache)
	}
	if len(r.Models) != 0 {
		t.Fatalf("empty window must have no models, got %+v", r.Models)
	}
}

// (5) NO IDENTITY ⇒ 403 (never client-supplied workspace for a non-admin key).
func TestUsage_NoWorkspaceIdentity_Forbidden(t *testing.T) {
	s := usageHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/api/usage?workspace_id=someone-else", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{})) // no ws, not admin
	rec := httptest.NewRecorder()
	s.handleUsage(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no identity must be 403, got %d (%s)", rec.Code, rec.Body.String())
	}
}
