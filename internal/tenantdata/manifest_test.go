package tenantdata

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// manifest_test.go — THE GUARD. The manifest is a list; this is what keeps it true.
//
// It reads the LIVE SCHEMA rather than a hardcoded table list, deliberately: every hardcoded count
// in this codebase has gone stale, usually within a day, and a stale deletion manifest is worse
// than none — it produces a confident claim that someone's data is gone when a table was missed.
// So the source of truth is information_schema, and this test fails when the schema grows a
// tenant-scoped table nobody classified.
//
// COST TO RUN: it needs a database with the migrations applied, so it is an INTEGRATION test, not
// a unit test. CI already provides one (LENS_TEST_DATABASE_URL, the pgvector service in ci.yaml),
// and it applies the real embedded migration chain into a private schema — the same harness the
// register-workspace and provision tests use. Without the variable it SKIPS, which means a
// developer running `go test ./...` on a laptop with no database does not get this coverage. That
// is a real limitation and the reason it must stay in CI's required set rather than being treated
// as an optional extra.

const manifestSchema = "lens_it_tenantdata"
const manifestSetupLock = 727274 // shared with the peer harnesses: serialises extension/catalog DDL

var manifestMigrateOnce sync.Once

// partitionSuffix matches the _p0.._p7 children of the partitioned audit tables. Partitions are
// collapsed to their parent: a DELETE against the parent reaches every partition, and listing the
// children would rot the moment the partition count changes.
var partitionSuffix = regexp.MustCompile(`_p\d+$`)

func schemaConn(t *testing.T) *pgx.Conn {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — the manifest guard needs a migrated database")
	}
	ctx := context.Background()

	manifestMigrateOnce.Do(func() {
		admin, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect for setup: %v", err)
		}
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, manifestSetupLock); err != nil {
			t.Fatalf("advisory lock: %v", err)
		}
		for _, stmt := range []string{
			`CREATE EXTENSION IF NOT EXISTS vector`,
			`DROP SCHEMA IF EXISTS ` + manifestSchema + ` CASCADE`,
			`CREATE SCHEMA ` + manifestSchema,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		_ = admin.Close(ctx)

		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cfg.RuntimeParams["search_path"] = manifestSchema + ",public"
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connect for migrate: %v", err)
		}
		defer conn.Close(ctx)
		if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
			t.Fatalf("apply migrations: %v", err)
		}
	})

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.RuntimeParams["search_path"] = manifestSchema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// liveTenantTables asks the SCHEMA which tables carry a workspace_id column, collapsing partitions.
func liveTenantTables(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT DISTINCT table_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND column_name = 'workspace_id'`, manifestSchema)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[partitionSuffix.ReplaceAllString(name, "")] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ⚠ THE GUARD. A tenant-scoped table nobody classified is a table a by-hand deletion will miss.
func TestManifestCoversEveryTenantScopedTable(t *testing.T) {
	conn := schemaConn(t)
	var missing []string
	for _, table := range liveTenantTables(t, conn) {
		if _, ok := Manifest[table]; !ok {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		t.Errorf(`%d tenant-scoped table(s) are NOT in the manifest: %s

A table with a workspace_id column holds a customer's data. Unclassified, it is a table the
by-hand deletion procedure will silently miss — which is worse than having no procedure, because
it produces a confident claim that someone's data is gone.

Add each to Manifest in internal/tenantdata/manifest.go with one of:
    Delete          — customer data, remove it
    Retain          — with the legal reason; only for audit-guarded rows that cannot be deleted
    NotTenantScoped — carries workspace_id but describes nobody; say why`,
			len(missing), strings.Join(missing, ", "))
	}
}

// The other direction: a manifest entry for a table that no longer exists is stale, and a stale
// entry in a deletion procedure is a DELETE against a missing table — which aborts the run at 2am.
func TestManifestHasNoEntriesForTablesThatNoLongerExist(t *testing.T) {
	conn := schemaConn(t)
	live := map[string]bool{}
	for _, n := range liveTenantTables(t, conn) {
		live[n] = true
	}
	for table := range Manifest {
		// `workspaces` is the MAPPING table: its key is `id`, not `workspace_id`, so the column
		// query above cannot discover it. It still belongs in the manifest and in DeleteOrder —
		// deleting it is what makes the retained ledger rows unlinkable — so it is exempt from
		// the staleness check rather than removed. (The guard found this itself: the first
		// version of the manifest failed here, which is the check working.)
		if table == "workspaces" {
			continue
		}
		if !live[table] {
			t.Errorf("manifest lists %q but no such tenant-scoped table exists — a DELETE against "+
				"it would abort the by-hand run partway through", table)
		}
	}
}

// A classification without a reason is an assertion. Retain and NotTenantScoped must justify
// themselves, so the next person can tell a decision from an oversight.
func TestRetainAndNotScopedEntriesCarryAReason(t *testing.T) {
	for table, e := range Manifest {
		if (e.Disposition == Retain || e.Disposition == NotTenantScoped) && strings.TrimSpace(e.Why) == "" {
			t.Errorf("%q is classified %v with no reason. Retaining or excluding a customer's data "+
				"needs a stated justification, not a bare classification.", table, e.Disposition)
		}
	}
}

// The delete order must end with the mapping. An interrupted run that removed `workspaces` first
// would leave orphaned rows and no way to find them again — the procedure could not be restarted.
func TestDeleteOrderEndsWithTheMapping(t *testing.T) {
	order := DeleteOrder()
	if len(order) == 0 {
		t.Fatal("DeleteOrder is empty")
	}
	if last := order[len(order)-1]; last != "workspaces" {
		t.Errorf("DeleteOrder ends with %q, want \"workspaces\" — the mapping must go last so an "+
			"interrupted run can be restarted", last)
	}
	for _, tbl := range order {
		if Manifest[tbl].Disposition != Delete {
			t.Errorf("DeleteOrder includes %q, which is not classified Delete", tbl)
		}
	}
}
