package proxy

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/migrations"
)

// Running every migration for each real-PG test was 77 runs and 70% of this package's time
// (B37.13), enough to push it past go test's timeout in CI. The migrations now run once, into a
// template database, and each test gets its own copy of it: still a freshly migrated, empty
// database per test, in about 40 ms instead of about a second.
var (
	migratedTemplateOnce sync.Once
	migratedTemplateName string
	migratedTemplateErr  error
	migratedDBSeq        atomic.Int64
)

// migratedDB creates a database holding every migration and no rows, returns its DSN, and drops
// it when the test ends. The caller has already skipped when LENS_TEST_DATABASE_URL is unset.
func migratedDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("LENS_TEST_DATABASE_URL")
	ctx := context.Background()
	migratedTemplateOnce.Do(func() {
		migratedTemplateName = fmt.Sprintf("lens_proxy_tpl_%d", time.Now().UnixNano())
		migratedTemplateErr = createMigratedTemplate(ctx, admin, migratedTemplateName)
	})
	if migratedTemplateErr != nil {
		t.Fatalf("migrated template: %v", migratedTemplateErr)
	}
	name := fmt.Sprintf("%s_%d", migratedTemplateName, migratedDBSeq.Add(1))
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer ac.Close(ctx)
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+migratedTemplateName); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() { dropDatabase(admin, name) })
	dsn, err := databaseURL(admin, name)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func createMigratedTemplate(ctx context.Context, admin, name string) error {
	ac, err := pgx.Connect(ctx, admin)
	if err != nil {
		return err
	}
	defer ac.Close(ctx)
	if _, err := ac.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	dsn, err := databaseURL(admin, name)
	if err != nil {
		return err
	}
	// A database with a session open cannot be copied, so this connection closes before any test clones it.
	mc, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer mc.Close(ctx)
	_, err = dbmigrate.Run(ctx, mc, migrations.FS)
	return err
}

// dropMigratedTemplate runs from TestMain once every test is done.
func dropMigratedTemplate() {
	if admin := os.Getenv("LENS_TEST_DATABASE_URL"); admin != "" && migratedTemplateName != "" {
		dropDatabase(admin, migratedTemplateName)
	}
}

func dropDatabase(admin, name string) {
	ctx := context.Background()
	if c, err := pgx.Connect(ctx, admin); err == nil {
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = c.Close(ctx)
	}
}

// databaseURL is admin pointed at database name, keeping its options (TestMain's search_path).
func databaseURL(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}
