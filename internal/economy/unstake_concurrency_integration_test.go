package economy

// WATCHED MONEY — the Unstake double-credit TOCTOU (real-Postgres proof).
//
// THE DEFECT (marketplace.go Unstake, pre-fix): the stake position is READ on the
// pool OUTSIDE any transaction and WITHOUT `FOR UPDATE`; the subsequent DELETE
// ignores RowsAffected; and the payout (principal + yield) is credited
// UNCONDITIONALLY. So two concurrent Unstake calls on the SAME owned, unlocked
// stake both pass the read, both "delete" (one really, one a 0-row no-op), and
// BOTH credit — principal+yield minted from a race. Same-tenant + ownership-gated,
// but it is real µLENS created from nothing.
//
// THE PROPERTY (asserted on the LEDGER, not the return values): after N=2
// concurrent Unstake on one stake, the payout is credited EXACTLY ONCE — exactly
// one `unstake` ledger row, the workspace balance rises by exactly the payout, the
// stake row is gone, and the losing call returns a clean already-unstaked error
// (ErrPositionNotFound → the handler's 404, never a 500). Looped 10× for
// flake-resistance. RED on unmodified main (double credit); GREEN after the fix.
//
// Skips cleanly without LENS_TEST_DATABASE_URL (the gated-integration convention).
// Test-only. Mirrors sec2_realpg_integration_test.go (own migrated private schema,
// disjoint from the package TestMain's lens_it_economy) + the fireConcurrent
// barrier idiom from povi/stakes_concurrency_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/migrations"
)

// unstakeRaceSchema pins this test to its OWN migrated private schema. The economy
// package TestMain points search_path at lens_it_economy (where other gated tests
// DROP/CREATE inline tables); applying the FULL migration set there would collide,
// so — exactly like sec2_realpg — we pin our own schema and override the pool's
// search_path via RuntimeParams (which wins over the env-var one).
const unstakeRaceSchema = "unstake_race_proof"

// unstakeRacePool applies the real migration set into unstakeRaceSchema and returns
// a pool pinned to it. The real schema (not inline DDL) guarantees the ledger's
// credit path behaves byte-for-byte like production (mint-type classification,
// BIGINT µLENS columns, constraints).
func unstakeRacePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG Unstake double-credit test")
	}
	ctx := context.Background()

	connCfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse conn config: %v", err)
	}
	connCfg.RuntimeParams["search_path"] = unstakeRaceSchema + ",public"
	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + unstakeRaceSchema + ` CASCADE`, `CREATE SCHEMA ` + unstakeRaceSchema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("apply real migrations: %v", err)
	}
	_ = conn.Close(ctx)

	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = unstakeRaceSchema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedUnlockedStake inserts ONE already-unlocked stake position with apy=0 (so the
// yield is deterministically 0 and the payout == principal exactly, µLENS-exact).
// lock_days=30 satisfies the table's CHECK (lock_days IN (30,90,180)); unlocks_at
// in the past makes it immediately unstakeable. Returns the position id.
func seedUnlockedStake(t *testing.T, pool *pgxpool.Pool, ws string, principal int64) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		INSERT INTO stake_positions (workspace_id, amount, lock_days, apy, unlocks_at)
		VALUES ($1, $2, 30, 0, NOW() - INTERVAL '1 day')
		RETURNING id`, ws, principal).Scan(&id)
	if err != nil {
		t.Fatalf("seed stake: %v", err)
	}
	return id
}

// unstakeLedger returns the count of `unstake` ledger rows and their summed amount
// for a workspace — the authoritative double-credit tripwire.
func unstakeLedger(t *testing.T, pool *pgxpool.Pool, ws string) (rows int, credited int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(SUM(amount), 0) FROM lens_token_ledger WHERE workspace_id=$1 AND type='unstake'`,
		ws).Scan(&rows, &credited); err != nil {
		t.Fatalf("read unstake ledger: %v", err)
	}
	return
}

func balanceOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(balance, 0) FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return bal
}

func stakeExists(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM stake_positions WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("read stake existence: %v", err)
	}
	return n > 0
}

// fireUnstakeConcurrent runs n copies of op behind a single release barrier so they
// contend as tightly as possible; returns each op's error.
func fireUnstakeConcurrent(n int, op func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = op(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// dumpUnstakeMoney returns a canonical one-line snapshot of EVERY money column the
// unstake path can touch, for a workspace — used for the byte-identical money-frozen
// proof (pristine main vs the fixed branch must match on the single-unstake path).
func dumpUnstakeMoney(t *testing.T, pool *pgxpool.Pool, ws string) string {
	t.Helper()
	ctx := context.Background()
	var rows int
	var amt, balAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(SUM(amount),0), COALESCE(MAX(balance_after),0)
		   FROM lens_token_ledger WHERE workspace_id=$1 AND type='unstake'`, ws).Scan(&rows, &amt, &balAfter); err != nil {
		t.Fatalf("dump ledger: %v", err)
	}
	var bal, held, locked, earned, spent int64
	err := pool.QueryRow(ctx,
		`SELECT balance, held_balance, locked_balance, lifetime_earned, lifetime_spent
		   FROM lens_token_balances WHERE workspace_id=$1`, ws).Scan(&bal, &held, &locked, &earned, &spent)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("dump balance: %v", err)
	}
	return fmt.Sprintf("ledger[rows=%d amount=%d balance_after=%d] balance[bal=%d held=%d locked=%d earned=%d spent=%d]",
		rows, amt, balAfter, bal, held, locked, earned, spent)
}

// TestUnstake_SingleHappyPath_MoneyFrozen_RealPG credits ONE unstake and pins every
// money column exactly. apy=0 ⇒ payout == principal (deterministic, µLENS-exact).
// The values are identical on pristine main and on the fixed branch — the single
// (uncontended) unstake path's money effect is unchanged; only the concurrent case
// differs. Run on both trees, the dump line is byte-identical.
func TestUnstake_SingleHappyPath_MoneyFrozen_RealPG(t *testing.T) {
	pool := unstakeRacePool(t)
	store := NewMarketplaceStore(mining.NewLedgerStore(pool), pool)
	ctx := context.Background()

	const principal int64 = 100_000_000
	const ws = "ws-unstake-frozen"
	id := seedUnlockedStake(t, pool, ws, principal)

	if err := store.Unstake(ctx, id, ws); err != nil {
		t.Fatalf("single unstake: %v", err)
	}

	dump := dumpUnstakeMoney(t, pool, ws)
	t.Logf("MONEY-FROZEN single-unstake dump: %s", dump)
	const want = "ledger[rows=1 amount=100000000 balance_after=100000000] balance[bal=100000000 held=0 locked=0 earned=100000000 spent=0]"
	if dump != want {
		t.Fatalf("money-frozen mismatch:\n got: %s\nwant: %s", dump, want)
	}
	if stakeExists(t, pool, id) {
		t.Fatalf("stake %s still present after single unstake", id)
	}
}

// TestUnstake_ConcurrentDoubleCredit_RealPG fires N=2 concurrent Unstake calls on
// ONE owned, unlocked stake and asserts the payout is credited EXACTLY ONCE — on the
// ledger. RED on unmodified main (both credit); GREEN once Unstake serializes on the
// row (SELECT … FOR UPDATE) and credits only when its DELETE removed the row.
func TestUnstake_ConcurrentDoubleCredit_RealPG(t *testing.T) {
	pool := unstakeRacePool(t)
	ledger := mining.NewLedgerStore(pool)
	store := NewMarketplaceStore(ledger, pool)
	ctx := context.Background()

	const principal int64 = 100_000_000 // 100 LENS in µLENS (SEC-2); apy=0 ⇒ payout == principal exactly
	const (
		iterations = 10 // flake-resistance: the race must never double-credit in any round
		N          = 2
	)

	for it := 0; it < iterations; it++ {
		ws := fmt.Sprintf("ws-unstake-race-%d", it)
		positionID := seedUnlockedStake(t, pool, ws, principal)

		errs := fireUnstakeConcurrent(N, func(int) error {
			return store.Unstake(ctx, positionID, ws)
		})

		// (1) LEDGER: exactly ONE payout row, summing to the payout exactly once.
		rows, credited := unstakeLedger(t, pool, ws)
		if rows != 1 {
			t.Fatalf("iter %d: DOUBLE CREDIT — got %d `unstake` ledger rows, want exactly 1 (credited=%d µLENS for one %d-µLENS stake)",
				it, rows, credited, principal)
		}
		if credited != principal {
			t.Fatalf("iter %d: credited %d µLENS, want exactly the payout %d (µLENS-exact, apy=0 ⇒ yield=0)", it, credited, principal)
		}

		// (2) BALANCE: rose by exactly one payout (from 0), never doubled.
		if bal := balanceOf(t, pool, ws); bal != principal {
			t.Fatalf("iter %d: balance=%d µLENS, want exactly one payout %d (double credit would be %d)", it, bal, principal, 2*principal)
		}

		// (3) the stake row is gone.
		if stakeExists(t, pool, positionID) {
			t.Fatalf("iter %d: stake %s still present after unstake", it, positionID)
		}

		// (4) EXACTLY one call succeeded; the loser returns a clean already-unstaked
		// error (ErrPositionNotFound → 404), never a 500-class error.
		var ok, notFound, other int
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, ErrPositionNotFound):
				notFound++
			default:
				other++
			}
		}
		if ok != 1 || notFound != 1 || other != 0 {
			t.Fatalf("iter %d: want exactly one success + one clean ErrPositionNotFound; got ok=%d notFound=%d other=%d (errs=%v)",
				it, ok, notFound, other, errs)
		}
	}
}
