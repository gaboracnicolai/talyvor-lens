package povi_test

// PoVI stake/slash concurrency — real-Postgres integration test (Gap 3).
//
// THE PROPERTY UNDER TEST. Per-node serialization is
// pg_advisory_xact_lock(hashtext(nodeID)) inside StakeManager.runStakeOp
// (stakes.go), and it is active ONLY when the manager is built with a non-nil db
// (the pool). Every existing UNIT test passes db=nil, so the lock that protects
// collateral — slashing BURNS real locked_balance LENS — is currently
// UNEXERCISED. This test proves the lock is load-bearing by running the SAME
// concurrent ops both ways against real PG, with the db arg as the only variable:
//
//	GREEN (db=pool): advisory lock LIVE  → povi_stakes stays in lockstep with the
//	                                       ledger's locked_balance; every invariant holds.
//	RED   (db=nil):  lock bypassed       → the (GET stake → ledger slash → PUT stake)
//	                                       read-modify-write loses updates: povi_stakes.amount
//	                                       / slashed_amount desync from the ledger's
//	                                       locked_balance (the record claims collateral the
//	                                       ledger has already burned; the audit under-counts).
//
// The ledger ops self-serialize via SELECT … FOR UPDATE, so locked_balance never
// goes negative on either path — the corruption the advisory lock prevents is the
// povi_stakes record racing the ledger, which is exactly what RED exposes.
//
// Skips cleanly without LENS_TEST_DATABASE_URL (the existing integration
// convention: mining/u6_integration_test.go, poolroyalty/adjudication_integration_test.go).
// Test-only: no production code changes (it exercises an existing lock).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/povi"
)

const concMinStake int64 = 1_000_000 // µLENS (SEC-2)
const (
	opTimeout = 30 * time.Second
	// concSchema isolates this test's fixtures in a private schema (see poviConcPool).
	concSchema = "povi_stake_conc_test"
)

// ── real-PG fixture ───────────────────────────────────────────

// poviConcPool opens the test pool and (re)creates exactly the tables the
// stake/slash path touches: lens_token_balances (+ locked_balance, mig 0032),
// lens_token_ledger (mig 0019), povi_stakes (mig 0032). Inline DDL mirrors the
// existing convention (mining/u6_integration_test.go).
func poviConcPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG PoVI stake/slash concurrency test")
	}
	ctx := context.Background()
	// Pin every pooled connection to a PRIVATE schema. ci.yaml runs the gated
	// integration tests together against ONE shared database on the premise that
	// they "touch disjoint tables"; this test reuses table names other gated tests
	// also create (lens_token_balances / lens_token_ledger), so a private
	// search_path keeps it genuinely disjoint and free of cross-package DROP/CREATE
	// races under `go test ./...`. All store/ledger SQL is unqualified, so it
	// resolves into this schema.
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = concSchema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + concSchema + ` CASCADE`,
		`CREATE SCHEMA ` + concSchema,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY,
			balance BIGINT NOT NULL DEFAULT 0,
			locked_balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0,
			lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id BIGSERIAL PRIMARY KEY,
			workspace_id TEXT NOT NULL, amount BIGINT NOT NULL,
			balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT, metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE povi_stakes (node_id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active',
			slashed_amount BIGINT NOT NULL DEFAULT 0,
			locked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			unbond_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema ddl: %v", err)
		}
	}
	return pool
}

func wsFor(node string) string { return "ws-" + node }

// newMgr builds a StakeManager over the REAL store + ledger. live=true passes the
// pool as db (advisory lock LIVE); live=false passes nil (lock bypassed — the RED
// path). unbondPeriod=0 so Release is immediately eligible for the anti-yank race.
func newMgr(pool *pgxpool.Pool, live bool) *povi.StakeManager {
	store := povi.NewNodeStakeStore(pool)
	ledger := mining.NewLedgerStore(pool)
	lookup := povi.NodeWorkspaceLookup(func(_ context.Context, n string) (string, error) {
		return wsFor(n), nil
	})
	if live {
		return povi.NewStakeManager(store, ledger, lookup, concMinStake, 0, pool)
	}
	return povi.NewStakeManager(store, ledger, lookup, concMinStake, 0, nil)
}

// ── balance / stake / ledger readers ──────────────────────────

func seedAvailable(t *testing.T, pool *pgxpool.Pool, ws string, amount int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO lens_token_balances (workspace_id, balance) VALUES ($1, $2)
		 ON CONFLICT (workspace_id) DO UPDATE SET balance = EXCLUDED.balance`, ws, amount)
	if err != nil {
		t.Fatalf("seed available: %v", err)
	}
}

type stakeRow struct {
	amount  int64
	slashed int64
	status  string
	exists  bool
}

func readStake(t *testing.T, pool *pgxpool.Pool, node string) stakeRow {
	t.Helper()
	var r stakeRow
	err := pool.QueryRow(context.Background(),
		`SELECT amount, slashed_amount, status FROM povi_stakes WHERE node_id=$1`, node).
		Scan(&r.amount, &r.slashed, &r.status)
	if errors.Is(err, pgx.ErrNoRows) {
		return r
	}
	if err != nil {
		t.Fatalf("read stake: %v", err)
	}
	r.exists = true
	return r
}

func readBalance(t *testing.T, pool *pgxpool.Pool, ws string) (available, locked int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT balance, locked_balance FROM lens_token_balances WHERE workspace_id=$1`, ws).
		Scan(&available, &locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0
	}
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return available, locked
}

// ledgerBurned is the real total burned out of locked: Σ(-amount) over the slash
// ledger rows (each slash records a negative delta).
func ledgerBurned(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var burned int64
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE workspace_id=$1 AND type=$2`,
		ws, mining.TypeStakeSlash).Scan(&burned)
	if err != nil {
		t.Fatalf("ledger burned: %v", err)
	}
	return burned
}

func approxEq(a, b int64) bool { return a == b }

// fireConcurrent runs n copies of op behind a single release barrier, so they
// contend as tightly as possible. Returns each op's error.
func fireConcurrent(n int, op func(i int) error) []error {
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

// ── concurrent-slash reconciliation (invariants 1, 2, 6) ──────

type slashRecon struct {
	amount     int64 // povi_stakes.amount
	slashedRec int64 // povi_stakes.slashed_amount (audit)
	locked     int64 // lens_token_balances.locked_balance (ledger truth)
	burnLedger int64 // Σ slash deltas in the ledger (real burn)
	sumReturns int64 // Σ successful StakeManager.Slash() returns
	okCount    int
}

// runConcurrentSlash seeds a node with stake S, then fires n concurrent Slash(f)
// through a manager built with the given liveness, and reads back the reconciled
// state. The initial Stake is ALWAYS done on a live manager so both halves start
// from the identical clean state — the only variable is the slash path's lock.
func runConcurrentSlash(t *testing.T, pool *pgxpool.Pool, node string, live bool, n int, f float64, S int64) slashRecon {
	t.Helper()
	ctx := context.Background()
	ws := wsFor(node)
	seedAvailable(t, pool, ws, 1_000_000_000_000)
	if _, err := newMgr(pool, true).Stake(ctx, node, S); err != nil {
		t.Fatalf("seed stake: %v", err)
	}

	mgr := newMgr(pool, live)
	returns := make([]int64, n)
	errs := fireConcurrent(n, func(i int) error {
		c, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		amt, err := mgr.Slash(c, node, f, "concurrent-slash")
		returns[i] = amt
		return err
	})

	st := readStake(t, pool, node)
	_, locked := readBalance(t, pool, ws)
	r := slashRecon{amount: st.amount, slashedRec: st.slashed, locked: locked, burnLedger: ledgerBurned(t, pool, ws)}
	for i, e := range errs {
		if e == nil {
			r.okCount++
			r.sumReturns += returns[i]
		}
	}
	return r
}

// TestPoVIStakeSlashConcurrency_RealPG is the headline RED/GREEN contrast plus the
// remaining invariants, all on real Postgres.
func TestPoVIStakeSlashConcurrency_RealPG(t *testing.T) {
	pool := poviConcPool(t)
	const S int64 = 100_000_000 // 100 LENS in µLENS (SEC-2)
	const (
		f = 0.5
		N = 16
	)

	// GREEN — advisory lock LIVE: the povi_stakes record stays in lockstep with the
	// ledger, the audit equals the real burn, and collateral is conserved.
	t.Run("GREEN_advisory_lock_holds_invariants", func(t *testing.T) {
		r := runConcurrentSlash(t, pool, "n-green", true, N, f, S)
		t.Logf("GREEN: amount=%d locked=%d slashed_rec=%d burn_ledger=%d Σreturns=%d ok=%d/%d",
			r.amount, r.locked, r.slashedRec, r.burnLedger, r.sumReturns, r.okCount, N)
		if r.amount < 0 {
			t.Fatalf("invariant: amount must never go negative; got %d", r.amount)
		}
		if !approxEq(r.amount, r.locked) {
			t.Fatalf("invariant 2 (locked_balance integrity): povi_stakes.amount=%d must equal ledger locked_balance=%d", r.amount, r.locked)
		}
		if !approxEq(r.slashedRec, r.burnLedger) {
			t.Fatalf("invariant 1 (audit): slashed_amount=%d must equal real burn=%d", r.slashedRec, r.burnLedger)
		}
		if !approxEq(r.sumReturns, r.burnLedger) {
			t.Fatalf("invariant 6 (supply): Σ Slash() returns=%d must equal ledger burn=%d", r.sumReturns, r.burnLedger)
		}
		if !approxEq(r.locked+r.burnLedger, S) {
			t.Fatalf("invariant 1 (conservation): remaining+burned=%d must equal seeded stake %d", r.locked+r.burnLedger, S)
		}
		if r.okCount != N {
			t.Fatalf("under serialization every slash should succeed; ok=%d/%d", r.okCount, N)
		}
	})

	// RED — lock bypassed (db=nil): the SAME concurrent slashes corrupt the record.
	// We require corruption to appear (proving the lock does real work); a fresh
	// node per attempt and a few retries make the contention-driven race reliable.
	// If it ever stays consistent, we FAIL LOUDLY — that would mean the advisory
	// lock is not the protecting mechanism and the premise needs re-examination.
	t.Run("RED_bypassed_lock_corrupts", func(t *testing.T) {
		const maxAttempts = 8
		var demonstrated bool
		var last slashRecon
		for attempt := 1; attempt <= maxAttempts && !demonstrated; attempt++ {
			r := runConcurrentSlash(t, pool, fmt.Sprintf("n-red-%d", attempt), false, N, f, S)
			last = r
			recordVsLedger := !approxEq(r.amount, r.locked)
			auditVsBurn := !approxEq(r.slashedRec, r.burnLedger)
			if recordVsLedger || auditVsBurn {
				demonstrated = true
				t.Logf("RED attempt %d CORRUPTED (as expected without the lock): "+
					"amount=%d vs ledger locked=%d (Δ=%d); slashed_amount(audit)=%d vs real burn=%d (Δ=%d). "+
					"The record claims collateral the ledger already burned / the audit under-counts.",
					attempt, r.amount, r.locked, r.amount-r.locked, r.slashedRec, r.burnLedger, r.slashedRec-r.burnLedger)
			} else {
				t.Logf("RED attempt %d stayed consistent (no race manifested this round); retrying", attempt)
			}
		}
		if !demonstrated {
			t.Fatalf("the lock-bypassed (db=nil) path stayed consistent across %d attempts "+
				"(last: amount=%d locked=%d slashed_rec=%d burn=%d) — the advisory lock may "+
				"NOT be the protecting mechanism; re-examine the premise",
				maxAttempts, last.amount, last.locked, last.slashedRec, last.burnLedger)
		}
	})

	// Invariant 3 — anti-yank: on an unbonding stake, a concurrent Slash and Release
	// race for the SAME collateral. Exactly ONE wins — never both — so burned+returned
	// equals the collateral, never double it. The advisory lock serializes them.
	t.Run("GREEN_anti_yank_slash_vs_release", func(t *testing.T) {
		ctx := context.Background()
		node := "n-antiyank"
		ws := wsFor(node)
		const A int64 = 100_000_000 // 100 LENS in µLENS
		seedAvailable(t, pool, ws, 1_000_000_000_000)
		mgr := newMgr(pool, true)
		if _, err := mgr.Stake(ctx, node, A); err != nil {
			t.Fatalf("stake: %v", err)
		}
		if err := mgr.Unbond(ctx, node); err != nil { // unbondPeriod=0 → releasable now, still slashable
			t.Fatalf("unbond: %v", err)
		}
		availAfterStake, _ := readBalance(t, pool, ws) // = 1_000_000 - A

		var slashErr, relErr error
		errs := fireConcurrent(2, func(i int) error {
			c, cancel := context.WithTimeout(ctx, opTimeout)
			defer cancel()
			if i == 0 {
				_, slashErr = mgr.Slash(c, node, 1.0, "anti-yank")
				return slashErr
			}
			relErr = mgr.Release(c, node)
			return relErr
		})
		_ = errs

		st := readStake(t, pool, node)
		avail, locked := readBalance(t, pool, ws)
		burned := ledgerBurned(t, pool, ws)
		returned := avail - availAfterStake // locked→available if Release won
		t.Logf("anti-yank: status=%s amount=%d locked=%d burned=%d returned=%d slashErr=%v relErr=%v",
			st.status, st.amount, locked, burned, returned, slashErr, relErr)

		if !approxEq(locked, 0) {
			t.Fatalf("collateral must fully leave the locked state exactly once; locked=%d", locked)
		}
		if !approxEq(burned+returned, A) {
			t.Fatalf("anti-yank: burned(%d)+returned(%d) must equal the collateral %d — no double-spend", burned, returned, A)
		}
		switch st.status {
		case string(povi.StakeSlashed):
			if !approxEq(burned, A) || !approxEq(returned, 0) {
				t.Fatalf("slash won: expect burned≈%d returned≈0; got burned=%d returned=%d", A, burned, returned)
			}
			if relErr == nil {
				t.Fatalf("a Release against a slashed stake must fail, got nil")
			}
		case string(povi.StakeReleased):
			if !approxEq(returned, A) || !approxEq(burned, 0) {
				t.Fatalf("release won: expect returned≈%d burned≈0; got returned=%d burned=%d", A, returned, burned)
			}
			if slashErr == nil {
				t.Fatalf("a Slash against a released stake must fail, got nil")
			}
		default:
			t.Fatalf("unexpected terminal status %q", st.status)
		}
	})

	// Invariant 4 — stake-while-slashing: concurrent top-ups and slashes. No lost
	// update: available drops by exactly Σtopups, the record tracks the ledger, and
	// global LENS (available+locked+burned) is conserved at the seeded total.
	t.Run("GREEN_stake_while_slashing", func(t *testing.T) {
		ctx := context.Background()
		node := "n-stakeslash"
		ws := wsFor(node)
		const (
			seed int64 = 1_000_000_000_000 // 1M LENS in µLENS (SEC-2)
			S    int64 = 100_000_000
			top  int64 = 10_000_000
		)
		const (
			T = 8
			K = 8
			f = 0.3
		)
		seedAvailable(t, pool, ws, seed)
		mgr := newMgr(pool, true)
		if _, err := mgr.Stake(ctx, node, S); err != nil {
			t.Fatalf("stake: %v", err)
		}

		topErrs := make([]error, T)
		slashErrs := make([]error, K)
		errs := fireConcurrent(T+K, func(i int) error {
			c, cancel := context.WithTimeout(ctx, opTimeout)
			defer cancel()
			if i < T {
				_, err := mgr.Stake(c, node, top)
				topErrs[i] = err
				return err
			}
			_, err := mgr.Slash(c, node, f, "stake-while-slashing")
			slashErrs[i-T] = err
			return err
		})
		_ = errs

		var sumTopups int64
		for i, e := range topErrs {
			if e == nil {
				sumTopups += top
			} else {
				t.Fatalf("top-up %d failed: %v", i, e)
			}
		}
		st := readStake(t, pool, node)
		avail, locked := readBalance(t, pool, ws)
		burned := ledgerBurned(t, pool, ws)
		t.Logf("stake-while-slashing: amount=%d locked=%d avail=%d burned=%d Σtopups=%d",
			st.amount, locked, avail, burned, sumTopups)

		if !approxEq(st.amount, locked) {
			t.Fatalf("record/ledger desync: amount=%d locked=%d", st.amount, locked)
		}
		if !approxEq(avail, seed-S-sumTopups) {
			t.Fatalf("lost update on available: got %d want %d (seed-S-Σtopups)", avail, seed-S-sumTopups)
		}
		if !approxEq(avail+locked+burned, seed) {
			t.Fatalf("conservation: available+locked+burned=%d must equal seeded %d", avail+locked+burned, seed)
		}
	})

	// Invariant 5 — terminal-state safety: once fully slashed, concurrent ops are
	// inert (error out) and cannot resurrect burned collateral.
	t.Run("GREEN_terminal_state_no_resurrection", func(t *testing.T) {
		ctx := context.Background()
		node := "n-terminal"
		ws := wsFor(node)
		seedAvailable(t, pool, ws, 1_000_000_000_000)
		mgr := newMgr(pool, true)
		if _, err := mgr.Stake(ctx, node, 100); err != nil {
			t.Fatalf("stake: %v", err)
		}
		if _, err := mgr.Slash(ctx, node, 1.0, "kill"); err != nil {
			t.Fatalf("full slash: %v", err)
		}
		before := readStake(t, pool, node)
		if before.status != string(povi.StakeSlashed) || !approxEq(before.amount, 0) {
			t.Fatalf("setup: expected fully-slashed terminal stake; got status=%s amount=%d", before.status, before.amount)
		}

		errs := fireConcurrent(3, func(i int) error {
			c, cancel := context.WithTimeout(ctx, opTimeout)
			defer cancel()
			switch i {
			case 0:
				_, err := mgr.Slash(c, node, 0.5, "post-terminal")
				return err
			case 1:
				return mgr.Unbond(c, node)
			default:
				return mgr.Release(c, node)
			}
		})
		for i, e := range errs {
			if e == nil {
				t.Fatalf("op %d against a slashed (terminal) stake must error, got nil", i)
			}
		}
		after := readStake(t, pool, node)
		_, locked := readBalance(t, pool, ws)
		if after.status != string(povi.StakeSlashed) || !approxEq(after.amount, 0) || !approxEq(locked, 0) {
			t.Fatalf("terminal stake mutated: status=%s amount=%d locked=%d", after.status, after.amount, locked)
		}
	})
}
