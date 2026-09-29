package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/moderatorkey"
	"github.com/talyvor/lens/migrations"
)

// B20.13 — the marketplace moderator key, created by the operator command and presented to the real
// auth.Manager, the real gates and the real store, on a migrated schema.

func moderatorKeyDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG moderator-key test (CI sets it; scripts/check-realpg-dsn.sh proves it is reachable)")
	}
	const schema = "cmd_moderator_key_realpg"
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

func TestModeratorKey_ReachesOnlyTheReviewQueue_EveryUseIsAudited_RevokedIs401(t *testing.T) {
	pool := moderatorKeyDB(t)
	ctx := context.Background()
	store := moderatorkey.NewStore(pool)

	// The operator creates the key with `lens moderator-keys create`; it is printed once.
	var out bytes.Buffer
	if err := moderatorKeysCommand(ctx, store, []string{"create", "web", "app"}, "operator-cli:test", &out); err != nil {
		t.Fatalf("create: %v", err)
	}
	raw := regexp.MustCompile(`tlv_mod_[0-9a-f]{48}`).FindString(out.String())
	if raw == "" {
		t.Fatalf("the command did not print a key:\n%s", out.String())
	}
	var stored string
	var keyID int64
	if err := pool.QueryRow(ctx, `SELECT id, key_hash FROM moderator_keys WHERE name = 'web app'`).Scan(&keyID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw {
		t.Fatal("the key is stored in the clear")
	}

	const adminKey = "test-admin-key"
	am := auth.NewManager(adminKey, nil, auth.New(pool), nil).WithModeratorKeys(store)
	reached := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusOK) })
	call := func(h http.Handler, method, path, key, operator string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		if operator != "" {
			req.Header.Set(moderatorOperatorHeader, operator)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// With the key, the review queue, approve and take-down are reached, and each use is recorded
	// with the operator the web app named.
	review := requireAdminOrModerator(am, store, handler)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/marketplace/review"},
		{http.MethodPost, "/v1/admin/marketplace/listings/lst_1/approve"},
		{http.MethodPost, "/v1/admin/marketplace/listings/lst_2/takedown"},
	}
	for _, rt := range routes {
		if code := call(review, rt.method, rt.path, raw, "nicolai@talyvor.com"); code != http.StatusOK {
			t.Fatalf("%s %s with the moderator key: %d, want 200", rt.method, rt.path, code)
		}
	}
	if code := call(review, http.MethodGet, routes[0].path, raw, ""); code != http.StatusBadRequest {
		t.Errorf("a moderator use that names no operator: %d, want 400", code)
	}
	uses, err := store.Uses(ctx, keyID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != len(routes) {
		t.Fatalf("audit log has %d uses, want %d (one per served request, none for the refused one)", len(uses), len(routes))
	}
	for i, u := range uses { // newest first
		rt := routes[len(routes)-1-i]
		if u.Operator != "nicolai@talyvor.com" || u.Method != rt.method || u.Path != rt.path {
			t.Errorf("use %d = %+v, want %s %s by nicolai@talyvor.com", i, u, rt.method, rt.path)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM moderator_key_uses`); err == nil {
		t.Error("the moderator audit log can be deleted from")
	}

	// Every other admin route answers 403 to it — the requireAdmin routes, the operator-read routes,
	// and the inline-gated /v1/admin routes behind the authenticated tenant middleware.
	others := map[string]http.Handler{
		"requireAdmin":               requireAdmin(am, handler),
		"requireAdminOrOperatorRead": requireAdminOrOperatorRead(am, handler),
		"the authed middleware":      auth.AuthMiddleware(auth.New(pool), am)(handler),
	}
	for name, h := range others {
		if code := call(h, http.MethodGet, "/v1/admin/workspaces", raw, "nicolai@talyvor.com"); code != http.StatusForbidden {
			t.Errorf("%s answered the moderator key %d, want 403", name, code)
		}
		// The control: the admin key still passes, so the 403 is the key and not a broken harness.
		if code := call(h, http.MethodGet, "/v1/admin/workspaces", adminKey, ""); code != http.StatusOK {
			t.Errorf("CONTROL: %s answered the admin key %d, want 200", name, code)
		}
	}

	// Revoked, it answers 401 from the next request.
	out.Reset()
	if err := moderatorKeysCommand(ctx, store, []string{"revoke", strconv.FormatInt(keyID, 10)}, "operator-cli:test", &out); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, rt := range routes {
		if code := call(review, rt.method, rt.path, raw, "nicolai@talyvor.com"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a revoked key: %d, want 401", rt.method, rt.path, code)
		}
	}
	if want := len(routes) + 3; reached != want { // 3 review uses + the 3 admin controls
		t.Errorf("handlers reached %d times, want %d", reached, want)
	}
}
