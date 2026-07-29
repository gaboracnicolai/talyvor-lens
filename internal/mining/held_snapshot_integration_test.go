package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HELD LENS MUST BE READABLE, or a correct system reads as a broken one.
//
// ⚠ THE INCIDENT. The first pool royalty ever minted landed as 822 µLENS with type
// pool_royalty_held; pool_royalty_mints agreed; and the workspace's LENS balance read 0. The 0 was
// CORRECT — a held mint credits lens_token_balances.held_balance and deliberately does NOT touch
// balance until internal/poolroyalty/sweeper.go settles it after the holdback window (72h by
// default). The tester's reaction was "this is weird", which is the right reaction to a screen
// showing one of three states.
//
// The column has existed since migration 0046 (and has been integer µLENS since 0082). What did
// not exist was any way to READ it: GetSnapshot selected balance, lifetime_earned, lifetime_spent
// and updated_at, and BalanceSnapshot had no field for held. So the suite could not have shown it
// however it wanted to — surfacing held needed this change first.
//
// Held is not spendable and not lost. A snapshot that reports only `balance` cannot express that
// third state, and every consumer downstream inherits the omission.

func heldSnapshotPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG held-snapshot test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	// Mirrors lens_token_balances after 0046 (held_balance) and 0082 (integer µLENS).
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS lens_token_balances (
			workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`TRUNCATE lens_token_balances`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return pool
}

// TestGetSnapshot_ReportsHeldSeparatelyFromSpendable is the reported state, exactly: a workspace
// whose only earnings are an unsettled royalty.
func TestGetSnapshot_ReportsHeldSeparatelyFromSpendable(t *testing.T) {
	pool := heldSnapshotPool(t)
	ctx := context.Background()
	// 822 µLENS held, nothing spendable — the live row.
	if _, err := pool.Exec(ctx,
		`INSERT INTO lens_token_balances (workspace_id, balance, held_balance, lifetime_earned)
		 VALUES ('ws-held', 0, 822, 822)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	snap, err := NewLedgerStore(pool).GetSnapshot(ctx, "ws-held")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.HeldBalance != 822 {
		t.Errorf("HeldBalance = %d, want 822. Without it a caller sees balance=0 and cannot tell "+
			"'earned nothing' from 'earned 822 that has not settled yet' — the two states that "+
			"made the first real royalty read as a bug.", snap.HeldBalance)
	}
	// ⚠ AND HELD MUST NOT BE FOLDED INTO SPENDABLE. Adding it to balance would be the other
	// failure: the screen would offer 822 the workspace cannot spend, and the conversion would
	// refuse with a number the user had just been shown.
	if snap.Balance != 0 {
		t.Errorf("Balance = %d, want 0 — held LENS is NOT spendable and must never be counted as "+
			"though it were", snap.Balance)
	}
}

// The ordinary case must be unchanged: a settled balance with nothing held reports zero held, not
// a missing field or a copy of the balance.
func TestGetSnapshot_NothingHeldReportsZero(t *testing.T) {
	pool := heldSnapshotPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO lens_token_balances (workspace_id, balance, held_balance, lifetime_earned)
		 VALUES ('ws-settled', 5000, 0, 5000)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	snap, err := NewLedgerStore(pool).GetSnapshot(ctx, "ws-settled")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Balance != 5000 || snap.HeldBalance != 0 {
		t.Errorf("balance=%d held=%d, want 5000/0", snap.Balance, snap.HeldBalance)
	}
}

// A workspace with no row at all is not an error and must not report a phantom held amount.
func TestGetSnapshot_UnknownWorkspaceHoldsNothing(t *testing.T) {
	pool := heldSnapshotPool(t)
	snap, err := NewLedgerStore(pool).GetSnapshot(context.Background(), "ws-never-seen")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Balance != 0 || snap.HeldBalance != 0 {
		t.Errorf("balance=%d held=%d, want 0/0", snap.Balance, snap.HeldBalance)
	}
}
