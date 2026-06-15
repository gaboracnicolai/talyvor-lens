package dbrouting

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPoolWithAppName builds a real-PG pool tagged with a distinct
// application_name, so a query through it reveals WHICH physical pool served it.
// Skips when LENS_TEST_DATABASE_URL is unset.
func testPoolWithAppName(t *testing.T, appName string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG read-routing test")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = appName
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func appNameOf(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), "SELECT current_setting('application_name')").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// TestReadPool_RoutesByConfig_RealPG — behaviorally proves ReadPool dispatches
// to the REPLICA connection when configured and the PRIMARY when not, observed
// through a per-pool application_name sentinel on a real query.
//
// LIMITATION (same as U7): this proves the ROUTING DECISION, not real
// replication lag — CI has a single Postgres, no streaming standby. Two pools
// over one DB stand in for primary+replica. Real lag-threshold behavior is not
// CI-covered (tracked as a follow-up: needs a streaming standby in CI).
func TestReadPool_RoutesByConfig_RealPG(t *testing.T) {
	primary := testPoolWithAppName(t, "u8-primary")
	replica := testPoolWithAppName(t, "u8-replica")

	// configured → analytics reads land on the REPLICA connection.
	if got := appNameOf(t, ReadPool(primary, replica)); got != "u8-replica" {
		t.Errorf("ReadPool(primary, replica) must route to the replica; app_name=%q", got)
	}
	// off-by-default (nil replica) → PRIMARY.
	if got := appNameOf(t, ReadPool(primary, nil)); got != "u8-primary" {
		t.Errorf("ReadPool(primary, nil) must route to the primary; app_name=%q", got)
	}
}

// TestOpenReplica_Valid_RealPG — a valid replica DSN yields a usable pool that
// serves reads (the flip-ON happy path).
func TestOpenReplica_Valid_RealPG(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG OpenReplica test")
	}
	pool := OpenReplica(context.Background(), url, ReplicaOpts{})
	if pool == nil {
		t.Fatal("a valid, reachable replica DSN must yield a non-nil pool")
	}
	defer pool.Close()
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("replica pool must serve reads: one=%d err=%v", one, err)
	}
}

// TestLagMonitor_Populates_RealPG — with a replica pool, sample() publishes a
// lag value (0 on the non-standby test PG, since pg_is_in_recovery()=false —
// the correct reading) and Check reports healthy with a lag detail. The
// no-replica → never-published case is pinned in TestLagMonitor_NoReplica_NoOp.
func TestLagMonitor_Populates_RealPG(t *testing.T) {
	pool := testPoolWithAppName(t, "u8-lag")
	var got float64
	called := false
	m := NewLagMonitor(pool, func(v float64) { got = v; called = true }, time.Second, nil)

	m.sample(context.Background())
	if !called {
		t.Fatal("sample must publish a lag value when a replica is configured (gauge populated)")
	}
	if got != 0 {
		t.Errorf("lag on a non-standby test PG must read 0 (not in recovery); got %v", got)
	}
	if ok, _, detail := m.Check(context.Background()); !ok {
		t.Errorf("Check on a reachable replica must be healthy; detail=%q", detail)
	}
}
