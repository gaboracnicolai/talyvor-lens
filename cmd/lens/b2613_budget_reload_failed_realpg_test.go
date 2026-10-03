package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

func b2613DB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG budget reload test")
	}
	const schema = "cmd_b2613_budget_reload_realpg"
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
	return pool
}

// B26.13 — a budget change whose reload fails answers 503 saying so, and the
// next refresh applies it. Until then the old limits are the ones in force.
func TestB2613_BudgetChangeThatFailsToLoadAnswers503_NextRefreshAppliesIt(t *testing.T) {
	ctx := context.Background()
	store := budgets.NewStore(b2613DB(t))
	svc := budgets.NewService(store)
	const ws = "ws-b2613"

	kept, err := store.Create(ctx, budgets.Budget{WorkspaceID: ws, Scope: budgets.ScopeWorkspace, LimitUSD: 10})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.Create(ctx, budgets.Budget{WorkspaceID: ws, Scope: budgets.ScopeSprint, ScopeID: "s1", LimitUSD: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}

	reloadDown := true
	reload := func(ctx context.Context) error {
		if reloadDown {
			return errors.New("budgets: active: conn refused")
		}
		return svc.Reload(ctx)
	}
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/budgets", budgetCreateHandler(store, reload))
	r.Patch("/v1/workspaces/{wsID}/budgets/{id}", budgetUpdateHandler(store, reload))
	r.Delete("/v1/workspaces/{wsID}/budgets/{id}", budgetDeleteHandler(store, reload))

	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	limits := func() map[string]float64 {
		got := map[string]float64{}
		for _, st := range svc.Status(ws, "t1", "s1") {
			got[string(st.Scope)] = st.LimitUSD
		}
		return got
	}

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/workspaces/" + ws + "/budgets", `{"scope":"team","scope_id":"t1","limit_usd":5}`},
		{http.MethodPatch, "/v1/workspaces/" + ws + "/budgets/" + kept.ID, `{"limit_usd":20,"alert_thresholds":[0.8]}`},
		{http.MethodDelete, "/v1/workspaces/" + ws + "/budgets/" + gone.ID, ``},
	} {
		code, body := do(c.method, c.path, c.body)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s with the reload failing: status %d, want 503 (body %v)", c.method, code, body)
		}
		if msg, _ := body["error"].(string); !strings.Contains(msg, "could not be reloaded") {
			t.Errorf("%s 503 does not name the failure: %q", c.method, msg)
		}
		if c.method != http.MethodDelete && body["budget"] == nil {
			t.Errorf("%s 503 does not return the saved budget: %v", c.method, body)
		}
	}
	if got, want := limits(), map[string]float64{"workspace": 10, "sprint": 3}; !sameLimits(got, want) {
		t.Fatalf("before the next refresh the old limits must still be in force: got %v, want %v", got, want)
	}

	// The next refresh is the periodic tick's svc.Reload.
	if err := svc.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := limits(), map[string]float64{"workspace": 20, "team": 5}; !sameLimits(got, want) {
		t.Fatalf("the next refresh did not apply the three changes: got %v, want %v", got, want)
	}
}

func sameLimits(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
