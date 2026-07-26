package earnverify

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// earnTestPool builds a real-PG pool with the minimal workspaces + lxc_purchases
// schema the predicate reads. Skips when LENS_TEST_DATABASE_URL is unset.
func earnTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG earn-verify test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS lxc_purchases`,
		`DROP TABLE IF EXISTS workspaces`,
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, earn_verified BOOLEAN NOT NULL DEFAULT false)`,
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0, livemode BOOLEAN)`, // BIGINT µLXC (0083); livemode NULLABLE (0109) — the fixture must mirror prod or it proves nothing
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

func mayEarn(t *testing.T, pool *pgxpool.Pool, ws string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := New(false).MayEarn(ctx, tx, ws)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// TestMayEarn_UnverifiedDenied — a free workspace (no purchase, earn_verified
// false) may NOT earn. This is the Sybil floor: a $0 identity cannot mint.
func TestMayEarn_UnverifiedDenied(t *testing.T) {
	pool := earnTestPool(t)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_u')`)
	if mayEarn(t, pool, "ws_u") {
		t.Error("a workspace with no purchase and earn_verified=false must NOT earn")
	}
	if mayEarn(t, pool, "ws_absent") {
		t.Error("a non-existent workspace must NOT earn")
	}
}

// TestMayEarn_AdminVouchAllowed — earn_verified=true (enterprise/self-host vouch).
func TestMayEarn_AdminVouchAllowed(t *testing.T) {
	pool := earnTestPool(t)
	exec(t, pool, `INSERT INTO workspaces (id, earn_verified) VALUES ('ws_v', true)`)
	if !mayEarn(t, pool, "ws_v") {
		t.Error("earn_verified=true (admin vouch) must allow earning")
	}
}

// TestMayEarn_CompletedPurchaseAllowed — a real-money completed purchase
// verifies, derived at read time (no column write).
func TestMayEarn_CompletedPurchaseAllowed(t *testing.T) {
	pool := earnTestPool(t)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_p')`)
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount, livemode) VALUES ('ws_p', 'completed', 100, true)`)
	if !mayEarn(t, pool, "ws_p") {
		t.Error("a completed real-money purchase must allow earning (read-time derive)")
	}
}

// TestMayEarn_RefundedAndAnomalousDenied — refunded (closes buy→refund→stay-
// verified) and anomalous (lxc_amount=0) purchases do NOT verify.
func TestMayEarn_RefundedAndAnomalousDenied(t *testing.T) {
	pool := earnTestPool(t)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_r'), ('ws_a')`)
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount) VALUES ('ws_r', 'refunded', 100)`)
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount) VALUES ('ws_a', 'anomalous', 0)`)
	if mayEarn(t, pool, "ws_r") {
		t.Error("a REFUNDED purchase must NOT verify (buy→refund→stay-verified loop)")
	}
	if mayEarn(t, pool, "ws_a") {
		t.Error("an ANOMALOUS purchase (lxc_amount=0) must NOT verify")
	}
}

// TestMayEarn_LivemodeRecordedAndRequired pins the 0109 property: a purchase must
// say which Stripe mode produced it, and the requireLive flag decides whether a
// TEST purchase confers earning rights.
//
// The trial depends on requireLive=false: a test-key purchase records
// livemode=false and still verifies, so a trial user becomes earn-verified by
// buying rather than by a manual UPDATE. Before open signup the flag flips to true
// and that door shuts — with no code change and no migration, which is why the mode
// is a recorded column and not an assumption.
func TestMayEarn_LivemodeRecordedAndRequired(t *testing.T) {
	pool := earnTestPool(t)
	exec(t, pool, `DELETE FROM lxc_purchases`)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_live')`)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_test')`)
	exec(t, pool, `INSERT INTO workspaces (id) VALUES ('ws_null')`)
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount, livemode) VALUES ('ws_live','completed',100,true)`)
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount, livemode) VALUES ('ws_test','completed',100,false)`)
	// A row from before 0109: mode never recorded.
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount) VALUES ('ws_null','completed',100)`)

	for _, tc := range []struct {
		name        string
		ws          string
		requireLive bool
		want        bool
		why         string
	}{
		{"trial: a TEST purchase verifies when live is not required", "ws_test", false, true,
			"the trial runs on test keys — a test purchase must earn-verify, or every trial user needs a manual UPDATE"},
		{"a LIVE purchase verifies either way", "ws_live", false, true, ""},
		{"a LIVE purchase still verifies when live IS required", "ws_live", true, true,
			"tightening the flag must not break real customers"},
		{"⚠ the door: a TEST purchase does NOT verify when live is required", "ws_test", true, false,
			"this is what stops free earning rights once signup is open"},
		{"a pre-0109 row (mode unrecorded) never verifies", "ws_null", false, false,
			"a row that cannot say it was real money must not be trusted to mean real money"},
		{"a pre-0109 row does not verify when live is required either", "ws_null", true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mayEarnLive(t, pool, tc.ws, tc.requireLive); got != tc.want {
				t.Errorf("MayEarn(%s, requireLive=%v) = %v, want %v — %s", tc.ws, tc.requireLive, got, tc.want, tc.why)
			}
		})
	}
}

// mayEarnLive is mayEarn with the requireLive flag exposed.
func mayEarnLive(t *testing.T, pool *pgxpool.Pool, ws string, requireLive bool) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ok, err := New(requireLive).MayEarn(ctx, tx, ws)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}
