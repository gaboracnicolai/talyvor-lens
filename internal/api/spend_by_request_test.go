package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
)

// T7 follow-up: /v1/api/spend/by-request — per-request read substrate. Tenancy-scoped identically to
// by-feature, consistent with the aggregate it un-rolls, and bounded (capped page + keyset cursor).

func spendHarness(t *testing.T) *Server {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG spend/by-request test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddl := `CREATE TABLE IF NOT EXISTS token_events (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '',
		feature TEXT, cost_usd FLOAT NOT NULL DEFAULT 0, input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMPTZ DEFAULT NOW(),
		serve_source TEXT NOT NULL DEFAULT 'upstream')`
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("schema: %v", err)
	}
	// The harness table predates 0100 in long-lived test databases — align it.
	if _, err := pool.Exec(context.Background(),
		`ALTER TABLE token_events ADD COLUMN IF NOT EXISTS serve_source TEXT NOT NULL DEFAULT 'upstream'`); err != nil {
		t.Fatalf("schema align: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE token_events`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return newServer(serverDeps{pool: pool})
}

func seedEvent(t *testing.T, s *Server, ws, reqID, feature string, cost float64, inTok, outTok int) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(),
		`INSERT INTO token_events (workspace_id, request_id, feature, cost_usd, input_tokens, output_tokens, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6, NOW())`, ws, reqID, feature, cost, inTok, outTok)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// call by-request as the given workspace (non-admin ⇒ scoped to ws), returning parsed rows + next_cursor.
func callByRequest(t *testing.T, s *Server, ws, query string) ([]map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/api/spend/by-request?"+query, nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: ws}))
	rec := httptest.NewRecorder()
	s.handleSpendByRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-request status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rows       []map[string]any `json:"rows"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Rows, resp.NextCursor
}

// (a) TENANCY: caller A sees ONLY A's request_ids; B's never appear.
func TestSpendByRequest_TenancyScoped(t *testing.T) {
	s := spendHarness(t)
	seedEvent(t, s, "wsA", "reqA1", "feat", 0.10, 100, 50)
	seedEvent(t, s, "wsA", "reqA2", "feat", 0.20, 100, 50)
	seedEvent(t, s, "wsB", "reqB1", "feat", 0.30, 100, 50)
	seedEvent(t, s, "wsB", "reqB2", "feat", 0.40, 100, 50)

	rows, _ := callByRequest(t, s, "wsA", "days=1&limit=100")
	got := map[string]bool{}
	for _, r := range rows {
		got[r["request_id"].(string)] = true
	}
	if !got["reqA1"] || !got["reqA2"] {
		t.Fatalf("A's requests must appear: got %v", got)
	}
	if got["reqB1"] || got["reqB2"] {
		t.Fatalf("TENANCY LEAK: B's request_ids appeared for caller A: %v", got)
	}
	if len(rows) != 2 {
		t.Fatalf("caller A must see exactly its 2 rows, got %d", len(rows))
	}
}

// (b) CONSISTENCY: SUM of per-request cost for a feature+window == the by-feature aggregate for the same
// feature+window. Proves the new surface reads the same data — no omission, no double-count.
func TestSpendByRequest_ConsistentWithByFeature(t *testing.T) {
	s := spendHarness(t)
	for i, c := range []float64{0.11, 0.22, 0.33, 0.44} {
		seedEvent(t, s, "wsC", fmt.Sprintf("r%d", i), "featX", c, 10, 5)
	}
	seedEvent(t, s, "wsC", "other", "featY", 9.99, 1, 1) // a different feature — must NOT be summed in

	// per-request sum for featX (walk all pages).
	var perReqSum float64
	cursor := ""
	for {
		q := "days=1&limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rows, next := callByRequest(t, s, "wsC", q)
		for _, r := range rows {
			if r["feature"] == "featX" {
				perReqSum += r["cost_usd"].(float64)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	// by-feature aggregate for featX (same window).
	req := httptest.NewRequest(http.MethodGet, "/v1/api/spend/by-feature?days=1", nil)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "wsC"}))
	rec := httptest.NewRecorder()
	s.handleSpendBy("feature")(rec, req)
	var agg []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode by-feature: %v", err)
	}
	var featXAgg float64
	for _, row := range agg {
		if row["feature"] == "featX" {
			featXAgg = row["cost_usd"].(float64)
		}
	}

	want := 0.11 + 0.22 + 0.33 + 0.44
	if diff := perReqSum - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("per-request sum=%.6f, want %.6f", perReqSum, want)
	}
	if diff := perReqSum - featXAgg; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("CONSISTENCY: per-request sum (%.6f) != by-feature aggregate (%.6f)", perReqSum, featXAgg)
	}
}

// (c) BOUNDED: more rows than one page ⇒ the endpoint caps the page and the cursor walks the rest with no
// dupes and no gaps.
func TestSpendByRequest_BoundedCursorWalk(t *testing.T) {
	s := spendHarness(t)
	const total = 12
	for i := 0; i < total; i++ {
		seedEvent(t, s, "wsD", fmt.Sprintf("req-%02d", i), "feat", 0.01, 1, 1)
	}

	const page = 5
	seen := map[string]int{}
	pages := 0
	cursor := ""
	for {
		q := fmt.Sprintf("days=1&limit=%d", page)
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rows, next := callByRequest(t, s, "wsD", q)
		pages++
		if len(rows) > page {
			t.Fatalf("page %d returned %d rows > cap %d", pages, len(rows), page)
		}
		for _, r := range rows {
			seen[r["request_id"].(string)]++
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("cursor did not terminate")
		}
	}

	if len(seen) != total {
		t.Fatalf("cursor walk must cover all %d rows exactly, got %d distinct", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("row %s returned %d times — dupe/gap in the cursor walk", id, n)
		}
	}
	if pages != 3 { // 5 + 5 + 2
		t.Fatalf("expected 3 pages (5+5+2), got %d", pages)
	}
}

// (e) CACHE-SERVE VISIBILITY (0100): every row carries serve_source so the dashboard can compute
// the cache hit rate from this endpoint alone — hits vs total, no inference. A cache row's
// cost_usd = 0 is TALYVOR'S provider cost; the requester's LXC debit is a different ledger, so the
// field lets a UI say "served from cache" instead of mis-rendering the request as free.
func TestSpendByRequest_ServeSourceSurfaced(t *testing.T) {
	s := spendHarness(t)
	seedEvent(t, s, "wsD", "req-up", "chat", 0.05, 100, 50)
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO token_events (workspace_id, request_id, feature, cost_usd, input_tokens, output_tokens, serve_source, created_at)
		 VALUES ('wsD','req-hit','chat',0,80,40,'cache_hit_exact', NOW())`); err != nil {
		t.Fatalf("seed cache row: %v", err)
	}

	rows, _ := callByRequest(t, s, "wsD", "days=1&limit=10")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	bySource := map[string]map[string]any{}
	for _, r := range rows {
		src, ok := r["serve_source"].(string)
		if !ok {
			t.Fatalf("row %v carries no serve_source — the hit rate is uncountable from this endpoint", r)
		}
		bySource[src] = r
	}
	up, hit := bySource["upstream"], bySource["cache_hit_exact"]
	if up == nil || hit == nil {
		t.Fatalf("want one upstream + one cache_hit_exact row, got sources %v", bySource)
	}
	if up["request_id"] != "req-up" || hit["request_id"] != "req-hit" {
		t.Errorf("source↔request mismatch: upstream=%v hit=%v", up["request_id"], hit["request_id"])
	}
	if hit["cost_usd"].(float64) != 0 {
		t.Errorf("cache row cost_usd = %v, want 0", hit["cost_usd"])
	}
}
