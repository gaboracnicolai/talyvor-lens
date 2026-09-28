package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/storedanswers"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// B17.1 — the synthetic routes, through the real handlers on a migrated schema.

func syntheticDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG synthetic-workspace test")
	}
	const schema = "cmd_synthetic_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
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
	return pool
}

func TestSynthetic_RoutesAre404WithoutTheKey(t *testing.T) {
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "", syntheticDeps{})
	for _, path := range []string{"/v1/synthetic/workspaces", "/v1/synthetic/workspaces/reset"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"count":100}`))
		req.Header.Set(syntheticKeyHeader, "anything")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("POST %s with LENS_SYNTHETIC_KEY unset = %d, want 404", path, w.Code)
		}
	}
}

func TestSynthetic_AHundredAreCreatedAndResetInOneCallEach(t *testing.T) {
	pool := syntheticDB(t)
	ctx := context.Background()
	ws := workspace.New(pool)
	dual := economy.NewDualTokenStore(nil, pool, nil)
	r := chi.NewRouter()
	mountSyntheticRoutes(r, "the-key", syntheticDeps{
		workspaces: ws,
		credits:    dual,
		answers:    storedanswers.New(pool, nil),
		audit:      pool,
		mint: func(workspaceID, _ string, _ []string, _ time.Duration) (string, error) {
			return "tok-" + workspaceID, nil
		},
	})
	call := func(path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set(syntheticKeyHeader, key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	one := func(sql string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}

	// A wrong key is refused, and the refusal is on the record.
	if w := call("/v1/synthetic/workspaces", "not-the-key", `{"count":100}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", w.Code)
	}
	if n := one(`SELECT count(*) FROM workspaces`); n != 0 {
		t.Fatalf("a refused call created %d workspaces", n)
	}

	// ONE call creates a hundred, each synthetic, pooling, and granted its test credits on the ledger.
	w := call("/v1/synthetic/workspaces", "the-key", `{"count":100}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	var out struct {
		Created    int             `json:"created"`
		Workspaces []syntheticUser `json:"workspaces"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Created != 100 || len(out.Workspaces) != 100 {
		t.Fatalf("create returned %d / %d workspaces (%v), want 100", out.Created, len(out.Workspaces), err)
	}
	if n := one(`SELECT count(*) FROM workspaces WHERE synthetic AND cache_poolable AND active`); n != 100 {
		t.Fatalf("%d synthetic pooling workspaces in the database, want 100", n)
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE type = $1 AND amount = $2 AND metadata->>'synthetic' = 'true'`,
		economy.LXCTypeGrant, syntheticCreditULXC); n != 100 {
		t.Fatalf("%d test-credit grants on the ledger, want 100 of %d µLXC", n, syntheticCreditULXC)
	}
	first := out.Workspaces[0].WorkspaceID
	if !ws.GetSynthetic(first) || out.Workspaces[0].Token != "tok-"+first {
		t.Fatalf("%s: synthetic=%v token=%q", first, ws.GetSynthetic(first), out.Workspaces[0].Token)
	}

	// The harness uses the first one: it spends credits and stores an answer.
	if err := dual.SpendLXC(ctx, first, 5_000_000, "a synthetic question"); err != nil {
		t.Fatalf("spend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO prompt_embeddings (provider, model, prompt_hash, embedding, response, workspace_id, embedding_model)
		VALUES ('anthropic', 'claude-haiku-4-5', 'h1', array_fill(0, ARRAY[1536])::vector, 'an answer', $1, 'text-embedding-3-small')`, first); err != nil {
		t.Fatalf("store an answer: %v", err)
	}

	// ONE call resets all hundred: the stored answer is gone and the credits are back.
	w = call("/v1/synthetic/workspaces/reset", "the-key", ``)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"reset":100`) {
		t.Fatalf("reset = %d %s, want 200 and 100 reset", w.Code, w.Body.String())
	}
	if n := one(`SELECT count(*) FROM prompt_embeddings WHERE workspace_id = $1`, first); n != 0 {
		t.Errorf("%d stored answers remain for %s after the reset", n, first)
	}
	if bal, err := dual.GetLXCBalance(ctx, first); err != nil || bal != syntheticCreditULXC {
		t.Errorf("%s balance after reset = %d (%v), want %d", first, bal, err, syntheticCreditULXC)
	}
	if n := one(`SELECT count(*) FROM lxc_ledger WHERE workspace_id = $1 AND type = $2 AND amount = 5000000`,
		first, economy.LXCTypeGrant); n != 1 {
		t.Errorf("%s has %d restoring grants of 5,000,000 µLXC on the ledger, want 1", first, n)
	}

	// Every call is on the record, the refused one included.
	if got := one(`SELECT count(*) FROM synthetic_operations`); got != 3 {
		t.Errorf("synthetic_operations holds %d calls, want 3", got)
	}
	if got := one(`SELECT count(*) FROM synthetic_operations WHERE outcome = 'unauthorized'`); got != 1 {
		t.Errorf("%d unauthorized calls recorded, want 1", got)
	}
}

// A synthetic workspace cannot buy, subscribe or convert: the three money routes call refuseSynthetic first.
func TestSynthetic_AWorkspaceWithTestCreditsCannotBuy(t *testing.T) {
	isSynthetic := func(ws string) bool { return ws == "s-test" }
	w := httptest.NewRecorder()
	if !refuseSynthetic(w, isSynthetic, "s-test") || w.Code != http.StatusForbidden {
		t.Errorf("synthetic workspace: refused=%v code=%d, want refused with 403", w.Code == http.StatusForbidden, w.Code)
	}
	if refuseSynthetic(httptest.NewRecorder(), isSynthetic, "u-real") {
		t.Error("a real workspace was refused a purchase")
	}
}
