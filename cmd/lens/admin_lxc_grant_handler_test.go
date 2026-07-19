package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/migrations"
)

// admin_lxc_grant_handler_test.go — the ADMIN-ONLY comped-LXC grant (POST /v1/admin/lxc/grant).
//
// A fresh workspace has 0 LXC and (without Stripe) no funding path, so it cannot transact. This
// endpoint funds it through economy.GrantLXC — the SAME atomic ledger+balance move as a purchase —
// recorded under LXCTypeGrant so a comp is never mistaken for paid revenue. requireAdmin at the route;
// default-off behind LENS_ADMIN_LXC_GRANT_ENABLED (off ⇒ route absent).

const lxcGrantSchema = "lens_it_lxcgrant"

var lxcGrantMigrateOnce sync.Once

// lxcGrantStore migrates the real schema into a private schema (see register_workspace_handler_test.go
// for why cmd/lens must isolate + provision pgvector in public) and returns a DualTokenStore over it.
// nil ledger + nil rate engine: GrantLXC only uses the pool.
func lxcGrantStore(t *testing.T) (*economy.DualTokenStore, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG admin LXC grant test")
	}
	ctx := context.Background()

	lxcGrantMigrateOnce.Do(func() {
		admin, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for setup: %v", err)
		}
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin setup tx: %v", err)
		}
		// Serialize cross-package setup (CREATE EXTENSION + schema DDL) under the peer advisory lock;
		// create the shared pgvector extension in PUBLIC (default search_path) so its type is visible
		// via the `,public` fallback from every private schema.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, wsRegisterSetupLock); err != nil {
			t.Fatalf("advisory lock: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			t.Fatalf("create extension: %v", err)
		}
		if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+lxcGrantSchema+` CASCADE`); err != nil {
			t.Fatalf("drop schema: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE SCHEMA `+lxcGrantSchema); err != nil {
			t.Fatalf("create schema: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit setup tx: %v", err)
		}
		_ = admin.Close(ctx)

		ccfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse migrate config: %v", err)
		}
		ccfg.RuntimeParams["search_path"] = lxcGrantSchema + ",public"
		conn, err := pgx.ConnectConfig(ctx, ccfg)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = lxcGrantSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return economy.NewDualTokenStore(nil, pool, nil), pool
}

// lxcState reads a workspace's µLXC balance and its grant-ledger rows (count + summed amount).
func lxcState(t *testing.T, pool *pgxpool.Pool, ws string) (balance int64, grantRows int, grantSum int64) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `SELECT COALESCE(balance,0) FROM lxc_balances WHERE workspace_id=$1`, ws).Scan(&balance); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read lxc balance: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(amount),0) FROM lxc_ledger WHERE workspace_id=$1 AND type='admin_grant'`, ws).
		Scan(&grantRows, &grantSum); err != nil {
		t.Fatalf("read grant ledger: %v", err)
	}
	return
}

func serveGrant(h http.Handler, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/v1/admin/lxc/grant", h)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/lxc/grant", strings.NewReader(body)))
	return w
}

var (
	adminAuthn    = fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}
	nonAdminAuthn = fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-attacker", IsAdmin: false}}
)

// TestAdminLXCGrant_AdminGrant_BalanceAndLedgerConsistent proves the money invariant: an admin grant
// of N µLXC moves the balance to exactly N (from 0) AND writes exactly ONE lxc_ledger row of type
// 'admin_grant' for N — atomically, integer µLXC, and self-describing (never 'purchase').
func TestAdminLXCGrant_AdminGrant_BalanceAndLedgerConsistent(t *testing.T) {
	store, pool := lxcGrantStore(t)
	const ws = "ws-grant-happy"
	const amt int64 = 250_000 // µLXC

	w := serveGrant(requireAdmin(adminAuthn, newAdminLXCGrantHandler(store)),
		`{"workspace_id":"`+ws+`","amount_ulxc":250000,"reason":"trial onboarding"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin grant: status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	bal, rows, sum := lxcState(t, pool, ws)
	if bal != amt {
		t.Fatalf("balance=%d µLXC, want exactly the grant %d", bal, amt)
	}
	if rows != 1 || sum != amt {
		t.Fatalf("grant ledger: rows=%d sum=%d µLXC, want exactly 1 row summing to %d", rows, sum, amt)
	}
	// The row must be self-describing as a comp — never counted as paid revenue.
	var purchases int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM lxc_ledger WHERE workspace_id=$1 AND type='purchase'`, ws).Scan(&purchases); err != nil {
		t.Fatal(err)
	}
	if purchases != 0 {
		t.Fatalf("a comped grant must NOT be recorded as a purchase; found %d purchase rows", purchases)
	}
}

// TestAdminLXCGrant_NonAdminRefused: requireAdmin refuses a non-admin caller (401) BEFORE the handler
// runs — no ledger row, balance untouched.
func TestAdminLXCGrant_NonAdminRefused(t *testing.T) {
	store, pool := lxcGrantStore(t)
	const ws = "ws-grant-nonadmin"

	w := serveGrant(requireAdmin(nonAdminAuthn, newAdminLXCGrantHandler(store)),
		`{"workspace_id":"`+ws+`","amount_ulxc":100000}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin grant: status=%d, want 401", w.Code)
	}
	if bal, rows, _ := lxcState(t, pool, ws); bal != 0 || rows != 0 {
		t.Fatalf("non-admin refusal must leave state untouched; got balance=%d rows=%d", bal, rows)
	}
}

// TestAdminLXCGrant_CrossTenant_NonAdminCannotGrantAnotherWorkspace: a non-admin key (ws-attacker)
// cannot mint LXC into ANOTHER workspace — requireAdmin 401s it and the target stays at zero.
func TestAdminLXCGrant_CrossTenant_NonAdminCannotGrantAnotherWorkspace(t *testing.T) {
	store, pool := lxcGrantStore(t)
	const target = "ws-grant-victim"

	w := serveGrant(requireAdmin(nonAdminAuthn, newAdminLXCGrantHandler(store)),
		`{"workspace_id":"`+target+`","amount_ulxc":999999}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cross-tenant non-admin grant: status=%d, want 401", w.Code)
	}
	if bal, rows, _ := lxcState(t, pool, target); bal != 0 || rows != 0 {
		t.Fatalf("cross-tenant grant must NOT credit the target; got balance=%d rows=%d", bal, rows)
	}
}

// TestAdminLXCGrant_PositiveAmountValidation: an admin cannot grant a non-positive amount — 400, no row.
func TestAdminLXCGrant_PositiveAmountValidation(t *testing.T) {
	store, pool := lxcGrantStore(t)
	const ws = "ws-grant-nonpos"
	for _, body := range []string{
		`{"workspace_id":"` + ws + `","amount_ulxc":0}`,
		`{"workspace_id":"` + ws + `","amount_ulxc":-5}`,
	} {
		w := serveGrant(requireAdmin(adminAuthn, newAdminLXCGrantHandler(store)), body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("non-positive grant %q: status=%d, want 400", body, w.Code)
		}
	}
	if bal, rows, _ := lxcState(t, pool, ws); bal != 0 || rows != 0 {
		t.Fatalf("rejected grant must write nothing; got balance=%d rows=%d", bal, rows)
	}
}

// TestAdminLXCGrantRoute_FlagGated pins that main.go registers POST /v1/admin/lxc/grant ONLY inside
// `if cfg.AdminLXCGrantEnabled` and behind requireAdmin — so flag-off ⇒ the route is not registered
// (chi 404). RED until the route is wired; mutation-teeth: drop the flag guard and this fails.
func TestAdminLXCGrantRoute_FlagGated(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !regexp.MustCompile(`if cfg\.AdminLXCGrantEnabled\s*\{`).Match(src) {
		t.Fatal("POST /v1/admin/lxc/grant must be registered inside `if cfg.AdminLXCGrantEnabled {` — default-off, route absent when the flag is off")
	}
	if !regexp.MustCompile(`"/v1/admin/lxc/grant",\s*requireAdmin\(authManager,\s*newAdminLXCGrantHandler\(`).Match(src) {
		t.Fatal("POST /v1/admin/lxc/grant must be wrapped in requireAdmin(authManager, newAdminLXCGrantHandler(...))")
	}
}
