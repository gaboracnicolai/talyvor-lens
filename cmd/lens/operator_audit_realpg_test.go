package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/internal/operatoraudit"
	"github.com/talyvor/lens/migrations"
)

// B27.28 — an operator action recorded through the endpoint, on the web app's moderator key, cannot be
// updated or deleted in the database, and the operator read key's CSV export returns it. Real routes,
// real gates, real store, migrated schema.

func operatorAuditDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG operator audit test")
	}
	const schema = "cmd_operator_audit_realpg"
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

func TestOperatorAudit_RecordedActionIsAppendOnlyAndExported(t *testing.T) {
	pool := operatorAuditDB(t)
	ctx := context.Background()
	moderators := moderatorkey.NewStore(pool)
	modKey, _, err := moderators.Create(ctx, "web app", "operator-cli:test")
	if err != nil {
		t.Fatal(err)
	}
	const readKey = "test-operator-read-key"
	am := auth.NewManager("test-admin-key", nil, auth.New(pool), nil).
		WithModeratorKeys(moderators).WithOperatorReadKey(readKey)
	trail := operatoraudit.NewStore(pool)
	r := chi.NewRouter()
	r.Post("/v1/admin/operator-audit/record", requireAdminOrModerator(am, moderators, newOperatorAuditRecordHandler(trail)))
	r.Get("/v1/admin/operator-audit", requireAdminOrOperatorRead(am, newOperatorAuditListHandler(trail)))
	r.Get("/v1/admin/operator-audit/export", requireAdminOrOperatorRead(am, newOperatorAuditExportHandler(trail)))
	call := func(method, path, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set(moderatorOperatorHeader, "nicolai@talyvor.com")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// The web app records a takedown on its moderator key. The detail opens with "=", which a
	// spreadsheet would run as a formula.
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	w := call(http.MethodPost, "/v1/admin/operator-audit/record", modKey,
		`{"action":"marketplace.takedown","target":"listing lst_9","detail":"=HYPERLINK(\"x\")","at":"`+at.Format(time.RFC3339)+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("record = %d %s; want 201", w.Code, w.Body)
	}
	var rec operatoraudit.Entry
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || rec.ID == 0 || rec.Actor != "nicolai@talyvor.com" {
		t.Fatalf("record answered %s (%v); want the stored entry under the named operator", w.Body, err)
	}
	// The read key reads; it does not write.
	if w := call(http.MethodPost, "/v1/admin/operator-audit/record", readKey, `{"action":"x"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("record on the operator read key = %d; want 401", w.Code)
	}

	// The database refuses to change or remove it, whatever the role.
	for _, stmt := range []string{
		`UPDATE operator_audit SET action = 'nothing happened' WHERE id = ` + strconv.FormatInt(rec.ID, 10),
		`DELETE FROM operator_audit WHERE id = ` + strconv.FormatInt(rec.ID, 10),
		`TRUNCATE operator_audit`,
	} {
		if _, err := pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v; want the append-only refusal", stmt, err)
		}
	}
	var n int
	var action string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(action) FROM operator_audit`).Scan(&n, &action); err != nil {
		t.Fatal(err)
	}
	if n != 1 || action != "marketplace.takedown" {
		t.Fatalf("operator_audit holds %d rows, action %q; want the one takedown, unchanged", n, action)
	}

	// The list filters by action.
	for q, want := range map[string]int{"?action=marketplace.takedown": 1, "?action=marketplace.approve": 0} {
		w := call(http.MethodGet, "/v1/admin/operator-audit"+q, readKey, "")
		var got struct{ Entries []operatoraudit.Entry }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Entries) != want {
			t.Errorf("list %s = %d %s; want %d entries", q, w.Code, w.Body, want)
		}
	}

	// The export returns it, as CSV, with the formula defused.
	w = call(http.MethodGet, "/v1/admin/operator-audit/export?actor=nicolai@talyvor.com&until="+at.Format("2006-01-02"), readKey, "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("export = %d %q %s; want 200 text/csv", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	rows, err := csv.NewReader(w.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{strconv.FormatInt(rec.ID, 10), at.Format(time.RFC3339), "nicolai@talyvor.com",
		"marketplace.takedown", "listing lst_9", `'=HYPERLINK("x")`}
	if len(rows) != 2 || strings.Join(rows[1][:6], "|") != strings.Join(want, "|") {
		t.Fatalf("export rows = %q; want the header and %q", rows, want)
	}
}
