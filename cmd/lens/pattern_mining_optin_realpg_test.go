package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/migrations"
)

// B18.54 — GET /v1/workspaces/{ws}/pattern-mining/opt-in says whether the workspace has opted in, read
// back from workspace_pattern_optin after an opt-in and an opt-out, and says when the deployment has
// pattern mining off.
func TestPatternMiningOptIn_TheReadFollowsTheTableThroughAnOptInAndAnOptOut(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG pattern-mining opt-in test")
	}
	const schema = "cmd_pattern_optin_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	miner := mining.NewPatternMiner(nil, pool)
	const ws = "ws-b1854"
	read := func(enabled bool) map[string]bool {
		t.Helper()
		r := chi.NewRouter()
		r.Get("/v1/workspaces/{wsID}/pattern-mining/opt-in", patternMiningOptInStatus(miner, enabled))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/pattern-mining/opt-in", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		var out map[string]bool
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET body %q: %v", w.Body.String(), err)
		}
		return out
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_pattern_optin WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if got := read(true); got["opted_in"] || !got["enabled"] || rows() != 0 {
		t.Fatalf("before any opt-in: %v with %d rows; want opted_in false, enabled true, no row", got, rows())
	}
	if err := miner.OptIn(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := read(true); !got["opted_in"] || rows() != 1 {
		t.Errorf("after opt-in: %v with %d rows; want opted_in true and one row", got, rows())
	}
	if got := read(false); got["enabled"] || !got["opted_in"] {
		t.Errorf("with pattern mining off: %v; want enabled false and the stored opt-in still reported", got)
	}
	if err := miner.OptOut(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := read(true); got["opted_in"] || rows() != 0 {
		t.Errorf("after opt-out: %v with %d rows; want opted_in false and no row", got, rows())
	}
}
