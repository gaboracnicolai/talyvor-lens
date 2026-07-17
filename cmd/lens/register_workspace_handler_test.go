package main

import (
	"context"
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
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/migrations"
)

// register_workspace_handler_test.go — the POST /v1/workspaces cross-tenant IDOR.
//
// RegisterWorkspace is a blind upsert on a body-supplied `id` (ON CONFLICT (id) DO UPDATE rewriting
// spend_limit_usd / cache_poolable / logging_policy / allowlists). The route carried no {wsID}
// segment (so workspaceIsolationMiddleware skipped it) and no admin gate — so ANY authenticated
// non-admin key could overwrite ANOTHER tenant's config. Fix: wrap the route in requireAdmin (the
// ~30-route control-plane sibling pattern); provisioning is privileged. The manager upsert and the
// bootstrap seed are untouched, and no money/ledger path is involved.

const wsVictim = "ws-victim-idor"

// attackBody: a non-admin tries to zero the victim's spend cap, force cross-tenant cache pooling,
// and blind the victim's audit logging.
const attackBody = `{"id":"ws-victim-idor","name":"x","spend_limit_usd":0,"cache_poolable":true,"logging_policy":"none"}`

// wsRegisterSchema isolates this test's TABLES in a private schema (the real `workspaces` row shape
// is built across 0005 + later ALTERs, so the test applies the SAME embedded migrations the server
// runs — drift-free — rather than hand-rolling it). Isolation matters two ways: (1) migrating the
// chain into `public` would populate public.schema_migrations, and since cmd/lens has no TestMain and
// runs BEFORE the internal integration packages in `go test ./...`, every later private-schema
// package would then see the chain as already-applied and skip creating its own tables; (2) this
// test's seeded rows never perturb another package's shared-table assertions. Mirrors the DB-backed
// peer harness (internal/*/schema_isolation_test.go).
const wsRegisterSchema = "lens_it_wsregister"

// wsRegisterSetupLock is the advisory-lock key the peer harness uses to serialize cross-package
// setup (CREATE EXTENSION + schema DDL) so concurrent package binaries can't race on the shared
// pgvector extension / catalog. Reused here so cmd/lens serializes against those peers too.
const wsRegisterSetupLock = 727274

var wsRegisterMigrateOnce sync.Once

func wsRegisterManager(t *testing.T) (*workspace.Manager, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG workspace-register IDOR test")
	}
	ctx := context.Background()

	wsRegisterMigrateOnce.Do(func() {
		// (1) Provision the shared pgvector extension into PUBLIC on a default-search_path
		// connection, serialized under the peer harness's advisory lock. The `vector` type from
		// 0001_init.sql is database-global and lands in the FIRST schema of search_path — so it
		// MUST be created here (public), not inside the private schema below, or the type is
		// stranded and invisible to the `,public` fallback in every other package. cmd/lens runs
		// first in `go test ./...`, so this is the run's first extension provisioner.
		admin, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for setup: %v", err)
		}
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin setup tx: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, wsRegisterSetupLock); err != nil {
			t.Fatalf("advisory lock: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			t.Fatalf("create extension: %v", err)
		}
		if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+wsRegisterSchema+` CASCADE`); err != nil {
			t.Fatalf("drop private schema: %v", err)
		}
		if _, err := tx.Exec(ctx, `CREATE SCHEMA `+wsRegisterSchema); err != nil {
			t.Fatalf("create private schema: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit setup tx: %v", err)
		}
		_ = admin.Close(ctx)

		// (2) Apply the full real migration chain into the PRIVATE schema. 0001's extension line
		// is now a no-op resolving to public; the tables (and this schema's own schema_migrations)
		// land in wsRegisterSchema, leaving public.schema_migrations untouched.
		ccfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse migrate config: %v", err)
		}
		ccfg.RuntimeParams["search_path"] = wsRegisterSchema + ",public"
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
	cfg.ConnConfig.RuntimeParams["search_path"] = wsRegisterSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return workspace.New(pool), pool
}

// seedVictim provisions ws_victim with a real spend cap, no pooling, full logging — the state a
// cross-tenant overwrite would trample.
func seedVictim(t *testing.T, reg *workspace.Manager, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id=$1`, wsVictim); err != nil {
		t.Fatalf("clean victim: %v", err)
	}
	if err := reg.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: wsVictim, Name: "victim", SpendLimitUSD: 100, CachePoolable: false, LoggingPolicy: workspace.LoggingFull,
	}); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if s, c, l := victimConfig(t, pool); s != 100 || c != false || l != "full" {
		t.Fatalf("seed precondition: got %v/%v/%q, want 100/false/full", s, c, l)
	}
}

func victimConfig(t *testing.T, pool *pgxpool.Pool) (spend float64, cachePoolable bool, logging string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT spend_limit_usd, cache_poolable, logging_policy FROM workspaces WHERE id=$1`, wsVictim).Scan(&spend, &cachePoolable, &logging); err != nil {
		t.Fatalf("read victim row: %v", err)
	}
	return
}

func serveRegister(h http.Handler, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/v1/workspaces", h)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workspaces", strings.NewReader(body)))
	return w
}

// TestWorkspaceRegister_Ungated_CrossTenantOverwriteLands documents the VULN: the bare handler (the
// route's behavior before the fix) lets a non-admin overwrite another workspace's config.
func TestWorkspaceRegister_Ungated_CrossTenantOverwriteLands(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	seedVictim(t, reg, pool)

	w := serveRegister(newRegisterWorkspaceHandler(reg), attackBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("ungated overwrite: status=%d, want 201 (the unguarded route accepts it)", w.Code)
	}
	if s, c, l := victimConfig(t, pool); s != 0 || c != true || l != "none" {
		t.Fatalf("VULN NOT REPRODUCED: victim config = %v/%v/%q, want OVERWRITTEN 0/true/none — the cross-tenant write must land without the gate", s, c, l)
	}
}

// TestWorkspaceRegister_AdminGated_CrossTenantRefused proves the FIX: wrapped in requireAdmin, a
// non-admin overwrite is refused 401 and the victim's config is UNCHANGED; an admin can still provision.
func TestWorkspaceRegister_AdminGated_CrossTenantRefused(t *testing.T) {
	reg, pool := wsRegisterManager(t)
	seedVictim(t, reg, pool)

	nonAdmin := fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-attacker", IsAdmin: false}}
	admin := fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}

	// NON-ADMIN → 401, victim config UNCHANGED.
	w := serveRegister(requireAdmin(nonAdmin, newRegisterWorkspaceHandler(reg)), attackBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin overwrite: status=%d, want 401 (admin credentials required)", w.Code)
	}
	if s, c, l := victimConfig(t, pool); s != 100 || c != false || l != "full" {
		t.Fatalf("CROSS-TENANT WRITE LANDED despite the gate: victim = %v/%v/%q, want UNCHANGED 100/false/full", s, c, l)
	}

	// ADMIN → 201: provisioning/registration still works for the operator.
	w = serveRegister(requireAdmin(admin, newRegisterWorkspaceHandler(reg)), `{"id":"ws-new-admin-idor","name":"n"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("admin register: status=%d, want 201 (provisioning must still work)", w.Code)
	}
}

// TestWorkspaceRegisterRoute_WiredWithRequireAdmin pins that main.go's POST /v1/workspaces route is
// actually wrapped in requireAdmin — RED until the fix lands; the mutation-teeth (remove the wrap)
// resurrects the cross-tenant overwrite and fails here.
func TestWorkspaceRegisterRoute_WiredWithRequireAdmin(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	re := regexp.MustCompile(`authed\.Post\("/v1/workspaces",\s*requireAdmin\(`)
	if !re.Match(src) {
		t.Fatal("POST /v1/workspaces must be wrapped in requireAdmin(authManager, …) — the cross-tenant IDOR gate is missing")
	}
}
