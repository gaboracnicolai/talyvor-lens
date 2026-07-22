package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

var cacheVisMigrateOnce sync.Once

// cacheVisPool migrates the real schema into a private schema and pins a pool to it — the standard
// LENS_TEST_DATABASE_URL idiom.
func cacheVisPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG cache-serve visibility test")
	}
	const schema = "proxy_cachevis_realpg"
	ctx := context.Background()
	cacheVisMigrateOnce.Do(func() {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = schema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
			if _, err := conn.Exec(ctx, ddl); err != nil {
				t.Fatalf("reset schema: %v", err)
			}
		}
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	})
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pool cfg: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// THE ACCEPTANCE PROPERTY, end to end against real PG: serve the same prompt twice through the
// full proxy path with the REAL alerts manager wired. The miss writes its unchanged 'upstream' row
// (cost > 0); the cache-served repeat writes a second row tagged 'cache_hit_exact' with cost_usd
// EXACTLY zero. Asserted on the rows in token_events, not on a status code. This is the live
// anomaly (billed, invisible) reproduced as a regression test — after the fix, visible.
func TestCacheServeVisibility_RealPG_HitWritesZeroCostRow_MissUnchanged(t *testing.T) {
	pool := cacheVisPool(t)
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	p.setAlertSink(alerts.New(pool, nil, nil)) // swap the counting fake for the real PG-backed manager

	// A local dispatch with a CATALOG-priced model (gpt-4o) — the miss row's cost > 0 assertion
	// below is real only when the price table knows the model (gpt-4 prices to 0).
	send := func() *httptest.ResponseRecorder {
		body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "ws-log")
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w
	}
	if w := send(); w.Code != http.StatusOK { // R1: miss → upstream
		t.Fatalf("miss dispatch: status %d, body=%s", w.Code, w.Body.String())
	}
	if w := send(); w.Code != http.StatusOK { // R2: identical → exact cache hit
		t.Fatalf("hit dispatch: status %d, body=%s", w.Code, w.Body.String())
	}

	rows, err := pool.Query(context.Background(),
		`SELECT serve_source, cost_usd, input_tokens, output_tokens, request_id
		 FROM token_events WHERE workspace_id = 'ws-log' ORDER BY created_at, serve_source`)
	if err != nil {
		t.Fatalf("query token_events: %v", err)
	}
	defer rows.Close()
	type ev struct {
		source    string
		cost      float64
		inT, outT int
		requestID string
	}
	var got []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.source, &e.cost, &e.inT, &e.outT, &e.requestID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("token_events rows = %d, want exactly 2 (one upstream miss + one tagged cache hit); got %+v", len(got), got)
	}
	miss, hit := got[0], got[1]
	if miss.source != "upstream" {
		t.Errorf("miss serve_source = %q, want upstream", miss.source)
	}
	if miss.cost <= 0 {
		t.Errorf("miss cost_usd = %v, want > 0 (a real upstream call is priced as before)", miss.cost)
	}
	if hit.source != "cache_hit_exact" {
		t.Errorf("hit serve_source = %q, want cache_hit_exact", hit.source)
	}
	if hit.cost != 0 {
		t.Errorf("hit cost_usd = %v, want exactly 0 — Talyvor's provider cost was zero (the requester's LXC debit is lxc_ledger's number, not this row's)", hit.cost)
	}
	if hit.inT <= 0 || hit.outT <= 0 {
		t.Errorf("hit tokens = %d/%d, want both > 0", hit.inT, hit.outT)
	}
	if hit.requestID == "" {
		t.Error("hit request_id empty — the cache row must be correlatable like every other row")
	}

	// The hit-rate query the dashboard will run — from this one table, no inference.
	var hits, total int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE serve_source LIKE 'cache_hit%'), COUNT(*)
		 FROM token_events WHERE workspace_id = 'ws-log'`).Scan(&hits, &total); err != nil {
		t.Fatalf("hit-rate query: %v", err)
	}
	if hits != 1 || total != 2 {
		t.Errorf("hit-rate substrate = %d/%d, want 1/2", hits, total)
	}
}
