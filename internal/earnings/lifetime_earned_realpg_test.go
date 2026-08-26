package earnings

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/mining"
)

// lifetime_earned_realpg_test.go — W4.6.1 step 7 asks for a surface behind the sentence "your plan
// is $20, your answers earned $6 of it back". `lens_token_balances.lifetime_earned` is the field
// anyone building that surface reaches for: it is named earned, it is already served on
// GET /v1/workspaces/{wsID}/tokens/balance as `lifetime_earned_ulens`, and cmd/node/earnings.go and
// cmd/cachenode/main.go already read it.
//
// ⚠ IT IS NOT EARNINGS. IT IS LIFETIME CREDITED. LedgerStore.apply does `earned += amount` on EVERY
// credit with NO filter on the ledger type, so LENS a workspace was GIVEN, BOUGHT, or simply got
// BACK all raise it. `Credit`'s own doc comment states the assumption that makes this safe —
// "Transfers move between workspaces via Transfer (not Credit), so this doesn't double-count" — and
// internal/economy/marketplace.go breaks it five times, calling CreditTx directly for unstake,
// marketplace_buy, marketplace_fee, marketplace_unsold_refund and marketplace_refund.
//
// EVERY ASSERTION BELOW IS EXPECTED TO GO RED WHEN THIS IS FIXED. That is the repair landing, and
// each failure message says so. Nothing here is a wish: it is a description of main today, made
// executable so it cannot go quiet.
//
// ⚠ NOT FIXED, AND DELIBERATELY. `lifetime_earned` is already served and already read by two node
// binaries, and narrowing it would change a number those binaries report. Which types SHOULD count
// as earned is the same product question step 7 is asking — see the queue entry.

func harness(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG lifetime_earned measurement")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The fixture mirrors migrations 0019 + 0032 + 0046 + 0036's CHECKs. It is NOT self-asserting:
	// every statement below is exercised by the REAL production SQL in internal/mining, so a column
	// this fixture is missing surfaces as a hard SQL error rather than as a passing test over a
	// narrower table. The CHECKs are carried across for the same reason — a fixture that is more
	// PERMISSIVE than production is the half a missing column would not catch.
	ctx := context.Background()
	for _, ddl := range []string{
		`DROP TABLE IF EXISTS lens_token_ledger`,
		`DROP TABLE IF EXISTS lens_token_balances`,
		`CREATE TABLE lens_token_ledger (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id TEXT NOT NULL,
			amount DOUBLE PRECISION NOT NULL,
			balance_after DOUBLE PRECISION NOT NULL,
			type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_balances (
			workspace_id TEXT PRIMARY KEY,
			balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_earned DOUBLE PRECISION NOT NULL DEFAULT 0,
			lifetime_spent DOUBLE PRECISION NOT NULL DEFAULT 0,
			locked_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			held_balance DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_token_balance_gte_zero CHECK (balance >= 0),
			CONSTRAINT chk_token_locked_balance_gte_zero CHECK (locked_balance >= 0),
			CONSTRAINT chk_token_held_balance_gte_zero CHECK (held_balance >= 0),
			CONSTRAINT chk_token_lifetime_earned_gte_zero CHECK (lifetime_earned >= 0))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("fixture DDL failed (%.60s…): %v", ddl, err)
		}
	}
	return pool
}

// earnedFromLedger sums only the ledger types internal/earnings classifies as SETTLED income —
// what "earned" means if the word is taken at face value.
// withTx runs fn inside a real transaction, the way every caller of the *Tx ledger methods does.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func earnedFromLedger(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT type, COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 GROUP BY type`, ws)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var total int64
	seen := 0
	for rows.Next() {
		var typ string
		var sum float64
		if err := rows.Scan(&typ, &sum); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if Classify(typ).Class == Settled {
			total += int64(sum)
		}
	}
	if seen == 0 {
		t.Fatalf("[HARNESS] the ledger has no rows for %s — every comparison below would be 0 vs 0", ws)
	}
	return total
}

func TestMeasured_LifetimeEarnedCountsLENSTheWorkspaceDidNotEarn(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	ctx := context.Background()
	const ws = "w461-h3n8-earned"

	// One genuine earning, and four credits that are not earnings by any reading. All four go
	// through the REAL production Credit path with the REAL type strings the shipped code uses.
	credits := []struct {
		typ    string
		amount int64
		earned bool // is this LENS the workspace EARNED?
	}{
		{mining.TypeCacheMine, 1_000_000, true},         // contributed a cached answer — real income
		{mining.TypeTransferIn, 5_000_000, false},       // somebody sent it LENS
		{"marketplace_buy", 7_000_000, false},           // it BOUGHT LENS
		{"marketplace_unsold_refund", 3_000_000, false}, // its own escrow came back
		{"unstake", 11_000_000, false},                  // its own principal came back
	}
	// ⚠ EACH TYPE GOES THROUGH THE ENTRY POINT PRODUCTION ACTUALLY USES FOR IT, so this cannot be
	// dismissed as a fixture driving the wrong door: internal/mining credits via Credit, and
	// internal/economy/marketplace.go credits via CreditTx inside its own transaction. Both funnel
	// into LedgerStore.applyTx — apply() only wraps it in a Begin/Commit — so the `earned += amount`
	// under test is one line reached two ways, and the test reaches it both ways.
	var creditedTotal, genuinelyEarned int64
	for _, c := range credits {
		var err error
		if c.earned {
			err = ls.Credit(ctx, ws, c.amount, c.typ, "w461 measurement", nil)
		} else {
			err = withTx(ctx, pool, func(tx pgx.Tx) error {
				return ls.CreditTx(ctx, tx, ws, c.amount, c.typ, "w461 measurement", nil)
			})
		}
		if err != nil {
			t.Fatalf("credit(%s): %v", c.typ, err)
		}
		creditedTotal += c.amount
		if c.earned {
			genuinelyEarned += c.amount
		}
	}

	snap, err := ls.GetSnapshot(ctx, ws)
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}

	// ⚠ ORDER AND FATALITY MATTER HERE, AND THE CONTROL IS WHY. These were two t.Fatalf calls with
	// the equality first, and control N3 (lifetime_earned never moves at all) proved the second one
	// UNREACHABLE: the equality always fires first and stops the test, so [W461-INFLATED] could not
	// fail under any mutation. An assertion that cannot fail is not an assertion. Both are t.Errorf
	// now, and the weaker claim is checked first, so each is independently observable.

	// (1) lifetime_earned exceeds what the workspace actually earned.
	if snap.LifetimeEarned <= genuinelyEarned {
		t.Errorf("[W461-INFLATED] lifetime_earned=%d is not above genuinely-earned=%d — the defect this "+
			"file measures is gone.", snap.LifetimeEarned, genuinelyEarned)
	}

	// (2) THE FINDING, stated as an equality so it cannot be satisfied by a near miss:
	//     lifetime_earned is EXACTLY the sum of every credit, earned or not.
	if snap.LifetimeEarned != creditedTotal {
		t.Errorf("[W461-CREDITED] lifetime_earned=%d, total credited=%d. If these have diverged, "+
			"LedgerStore.applyTx has learned to tell an earning from a credit — THE DEFECT IS FIXED and "+
			"this measurement should be replaced by a guard on the new rule.",
			snap.LifetimeEarned, creditedTotal)
	}

	fromLedger := earnedFromLedger(t, pool, ws)
	if fromLedger != genuinelyEarned {
		t.Errorf("[W461-CLASSIFY] internal/earnings summed %d as settled income but only %d was earned — "+
			"the classification and this fixture disagree, and one of them is wrong", fromLedger, genuinelyEarned)
	}
	t.Logf("MEASURED: lifetime_earned=%d µLENS; genuinely earned=%d µLENS; overstatement=%d µLENS (%.1f×)",
		snap.LifetimeEarned, genuinelyEarned, snap.LifetimeEarned-genuinelyEarned,
		float64(snap.LifetimeEarned)/float64(genuinelyEarned))
}

// TestMeasured_AStakeRoundTripInflatesLifetimeEarnedWithoutBound is the sharpest form of the same
// defect: it needs no counterparty and no market. Staking your own LENS and unstaking it returns you
// to exactly the balance you started with — and raises lifetime_earned by the principal EVERY TIME.
func TestMeasured_AStakeRoundTripInflatesLifetimeEarnedWithoutBound(t *testing.T) {
	pool := harness(t)
	ls := mining.NewLedgerStore(pool)
	ctx := context.Background()
	const ws = "w461-h3n8-roundtrip"
	const principal = 4_000_000

	// Seed the wallet with a genuine earning so there is something to stake.
	if err := ls.Credit(ctx, ws, principal, mining.TypeCacheMine, "seed", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	start, err := ls.GetSnapshot(ctx, ws)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The exact type strings internal/economy/marketplace.go uses for Stake/Unstake.
	// DebitTx / CreditTx inside a caller-owned transaction — exactly how
	// internal/economy/marketplace.go's Stake and Unstake move the principal.
	const cycles = 5
	for i := 0; i < cycles; i++ {
		if err := withTx(ctx, pool, func(tx pgx.Tx) error {
			return ls.DebitTx(ctx, tx, ws, principal, "stake", "LENS staked for yield", nil)
		}); err != nil {
			t.Fatalf("stake cycle %d: %v", i, err)
		}
		if err := withTx(ctx, pool, func(tx pgx.Tx) error {
			return ls.CreditTx(ctx, tx, ws, principal, "unstake", "stake principal returned", nil)
		}); err != nil {
			t.Fatalf("unstake cycle %d: %v", i, err)
		}
	}

	end, err := ls.GetSnapshot(ctx, ws)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// The wallet is exactly where it started — no value was created.
	if end.Balance != start.Balance {
		t.Fatalf("[W461-RT-BALANCE] the round trip moved the balance %d → %d, so this is not the "+
			"value-neutral loop the finding depends on", start.Balance, end.Balance)
	}
	// lifetime_earned grew by the principal on every cycle.
	want := start.LifetimeEarned + cycles*principal
	if end.LifetimeEarned != want {
		t.Fatalf("[W461-RT] after %d value-neutral stake/unstake cycles lifetime_earned is %d, expected %d "+
			"(start %d + %d×%d). If it stayed at %d, unstake no longer counts as earned — THE DEFECT IS FIXED.",
			cycles, end.LifetimeEarned, want, start.LifetimeEarned, cycles, principal, start.LifetimeEarned)
	}
	t.Logf("MEASURED: %d value-neutral cycles raised lifetime_earned %d → %d µLENS while the balance "+
		"stayed at %d. The field is unbounded in the number of round trips.",
		cycles, start.LifetimeEarned, end.LifetimeEarned, end.Balance)
}
