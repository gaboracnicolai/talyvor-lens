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
		`CREATE TABLE lxc_purchases (workspace_id TEXT NOT NULL, status TEXT NOT NULL, lxc_amount BIGINT NOT NULL DEFAULT 0)`, // BIGINT µLXC (matches prod 0083)
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
	ok, err := New().MayEarn(ctx, tx, ws)
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
	exec(t, pool, `INSERT INTO lxc_purchases (workspace_id, status, lxc_amount) VALUES ('ws_p', 'completed', 100)`)
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
