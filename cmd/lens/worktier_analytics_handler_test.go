package main

// Proofs for the WorkTier analytics endpoint. Real-PG (the Aggregate SQL + tenant isolation
// can't be shown by a fake) for functional/isolation/window; unit (fakes) for admin-gate +
// required-param. Reuses fakeAuthenticator from adjudication_handler_test.go (same package).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/worktier"
)

func wtAdminAuth() fakeAuthenticator {
	return fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin1"}}
}

// fakeWTAggregator records whether Aggregate was reached (for the gate/required-param proofs).
type fakeWTAggregator struct {
	called bool
	rows   []worktier.TierCount
	err    error
}

func (f *fakeWTAggregator) Aggregate(_ context.Context, _ string, _ time.Duration) ([]worktier.TierCount, error) {
	f.called = true
	return f.rows, f.err
}

func wtAnalyticsHarness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG worktier analytics test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS work_tier_observations`,
		`CREATE TABLE work_tier_observations (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL,
		    feature TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		    size_bucket TEXT NOT NULL, cost_bucket TEXT NOT NULL, complexity TEXT NOT NULL, sensitivity TEXT NOT NULL,
		    input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		    cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0, complexity_score INTEGER NOT NULL DEFAULT 0,
		    pii_detected BOOLEAN NOT NULL DEFAULT FALSE, guardrail_fired BOOLEAN NOT NULL DEFAULT FALSE,
		    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func seedWT(t *testing.T, pool *pgxpool.Pool, ws, model, size, cost, complexity, sensitivity string, ageHours int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO work_tier_observations (workspace_id, model, size_bucket, cost_bucket, complexity, sensitivity, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6, now() - make_interval(hours => $7))`,
		ws, model, size, cost, complexity, sensitivity, ageHours); err != nil {
		t.Fatalf("seed wt: %v", err)
	}
}

func getDistribution(t *testing.T, store worktierAggregator, target string) (*httptest.ResponseRecorder, workTierDistributionResponse) {
	t.Helper()
	h := requireAdmin(wtAdminAuth(), http.HandlerFunc(newWorkTierDistributionHandler(store)))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var resp workTierDistributionResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v; body %s", err, rec.Body.String())
		}
	}
	return rec, resp
}

// (1)+(2) FUNCTIONAL + TENANT ISOLATION — A's distribution only, B never appears.
func TestWorkTierDistribution_FunctionalAndTenantIsolation_Integration(t *testing.T) {
	pool := wtAnalyticsHarness(t)
	// A: 2 of (gpt-4, large/high/complex/normal) + 1 of (gpt-4, small/low/simple/normal).
	seedWT(t, pool, "A", "gpt-4", "large", "high", "complex", "normal", 1)
	seedWT(t, pool, "A", "gpt-4", "large", "high", "complex", "normal", 1)
	seedWT(t, pool, "A", "gpt-4", "small", "low", "simple", "normal", 1)
	// B: 5 of a DISTINCT model/tier — must never leak into A's response.
	for i := 0; i < 5; i++ {
		seedWT(t, pool, "B", "claude-secret-model", "medium", "moderate", "moderate", "elevated", 1)
	}

	rec, resp := getDistribution(t, worktier.NewStore(pool), "/v1/admin/worktier/distribution?workspace_id=A")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d want 200; body %s", rec.Code, rec.Body.String())
	}
	if resp.WorkspaceID != "A" {
		t.Errorf("workspace_id %q want A", resp.WorkspaceID)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("A rows %d want 2: %+v", len(resp.Rows), resp.Rows)
	}
	// ORDER BY count DESC → the count-2 cell first.
	if resp.Rows[0].Count != 2 || resp.Rows[0].SizeBucket != "large" || resp.Rows[0].CostBucket != "high" || resp.Rows[0].Complexity != "complex" {
		t.Errorf("top row %+v want count 2 / large / high / complex", resp.Rows[0])
	}
	if resp.Rows[1].Count != 1 || resp.Rows[1].SizeBucket != "small" {
		t.Errorf("second row %+v want count 1 / small", resp.Rows[1])
	}
	// TENANT ISOLATION: none of B's distinctive values anywhere in A's response.
	body := rec.Body.String()
	for _, bMark := range []string{"claude-secret-model", "elevated", "moderate", "medium"} {
		if strings.Contains(body, bMark) {
			t.Errorf("workspace A response LEAKED workspace B value %q: %s", bMark, body)
		}
	}
	for _, r := range resp.Rows {
		if r.Model != "gpt-4" {
			t.Errorf("A response contains non-A model %q — tenant isolation breach", r.Model)
		}
	}
}

// (3) REQUIRED PARAM — no workspace_id → 400, Aggregate never reached (no cross-tenant blob path).
func TestWorkTierDistribution_RequiresWorkspaceID(t *testing.T) {
	store := &fakeWTAggregator{}
	rec, _ := getDistribution(t, store, "/v1/admin/worktier/distribution") // no workspace_id
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no workspace_id: code %d want 400", rec.Code)
	}
	if store.called {
		t.Error("Aggregate reached without workspace_id — must 400 first (no cross-tenant mode)")
	}
}

// (4) ADMIN-GATE — non-admin + unauthenticated → 401, NO data leaked, Aggregate never reached.
func TestWorkTierDistribution_AdminGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    fakeAuthenticator
	}{
		{"non-admin", fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: false, UserID: "ws-7"}}},
		{"unauthenticated", fakeAuthenticator{err: http.ErrNoCookie}},
	} {
		store := &fakeWTAggregator{rows: []worktier.TierCount{{Model: "leak-model", Count: 9}}}
		h := requireAdmin(tc.a, http.HandlerFunc(newWorkTierDistributionHandler(store)))
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/worktier/distribution?workspace_id=A", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code %d want 401", tc.name, rec.Code)
		}
		if store.called {
			t.Errorf("%s: Aggregate reached despite 401 — no read may pass the gate", tc.name)
		}
		body := rec.Body.String()
		for _, leak := range []string{"leak-model", "size_bucket", "\"rows\""} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: 401 body leaked %q: %s", tc.name, leak, body)
			}
		}
	}
}

// (5) WINDOW — created_at outside the window is excluded; a wider window includes it.
func TestWorkTierDistribution_WindowFilters_Integration(t *testing.T) {
	pool := wtAnalyticsHarness(t)
	seedWT(t, pool, "A", "gpt-4", "large", "high", "complex", "normal", 1) // 1h ago — inside 24h
	seedWT(t, pool, "A", "gpt-4", "small", "low", "simple", "normal", 100) // 100h ago — outside 24h
	store := worktier.NewStore(pool)

	_, resp := getDistribution(t, store, "/v1/admin/worktier/distribution?workspace_id=A") // default 24h
	if len(resp.Rows) != 1 || resp.Rows[0].SizeBucket != "large" {
		t.Errorf("default 24h window: rows %+v, want only the recent large row", resp.Rows)
	}
	_, resp2 := getDistribution(t, store, "/v1/admin/worktier/distribution?workspace_id=A&window=200h")
	if len(resp2.Rows) != 2 {
		t.Errorf("200h window: rows %d want 2 (both rows in range)", len(resp2.Rows))
	}
}
