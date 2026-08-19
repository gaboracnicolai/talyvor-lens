package poolroyalty

// bonded_settlement_integration_test.go — THE CLAIM ROW AND THE HELD BALANCE DISAGREE
// WHEN A MINT IS BONDED (real PG, real minter, real sweeper).
//
// Every held mint is committed as TWO facts in ONE transaction:
//
//	(a) the claim row  — pool_royalty_mints.minted_amount, written by the minter
//	(b) the held credit — lens_token_balances.held_balance, written by CreditHeldTx
//
// The finalize sweeper settles (a): it reads minted_amount and asks FinalizeHeldTxAs to
// move that many µLENS out of (b). So (a) and (b) MUST agree, and nothing enforces it.
//
// P1 #9 (reputation bond) and KE-2 (drift haircut) both REDUCE (b) inside CreditHeldTx —
// after the minter has already written (a) with the unreduced base. MEASURED through the
// real MintServedHit + the real FinalizeSweeper on real Postgres, contributor R=0.425 so
// f(R)=0.5 exactly:
//
//	base 10_000_000 µLENS  →  held_balance 5_000_000  →  claim.minted_amount 10_000_000
//	sweep: settled=0, "mining: insufficient held balance", row stays status='held'
//
// A bonded pool-royalty mint therefore CANNOT SETTLE ON ITS OWN. It is retried every tick,
// forever, WARN-logged each time; RunOnce returns nil, so the failure is invisible to the
// caller and to metrics. The contributor's LENS never becomes spendable, never enters
// supply (pool_royalty_held is supply-excluded by design), and never recognizes as
// lifetime_earned. With a SECOND bonded mint in flight the first one settles instead — at
// its FULL UNSCALED amount, consuming the second's held — and the second strands.
//
// ⚠ THE SAFETY ARGUMENT THAT LET THIS FLAG OUT OF THE KILL SWITCH IS THE ONE THIS
// FALSIFIES. config.go says of ReputationBondedMintingEnabled: "It can only REDUCE or
// BLOCK a mint the U6 floor/stake/rate-cap already allowed — never enable one — so it is
// deliberately NOT in the kill-switch force-off block." Reducing a mint is exactly what it
// does, and on this track reducing a mint is not a smaller payout, it is an unsettleable
// one. The reducer/enabler distinction is sound; it is not the distinction that decides
// whether flipping the flag is safe.
//
// ⚠ NOT LIVE TODAY, AND ONE ENV VAR FROM IT: LENS_REPUTATION_BONDED_MINTING_ENABLED and
// the KE-2 haircut both default OFF (parseBoolEnv, no default-on loop), so with the bond
// off the two facts agree and everything settles — which is why every existing sweeper
// test is green. They all seed the claim row with the same number they hand CreditHeldTx,
// and none of them arms the bond.
//
// ⚠ THE BLOCK PATH IS CLEAN, MEASURED SEPARATELY: below the access floor CreditHeldTx
// returns ErrReputationFloor, the minter's deferred rollback discards the claim row too,
// and the measurement shows 0 claim rows / 0 held. The defect is specific to the SCALE
// path — a mint that is reduced but not refused.
//
// WHAT THIS FILE PINS, AND WHY IT IS GREEN RATHER THAN RED: the repair is a money-path
// DECISION, not a typo (QUEUE.md W6.1). minted_amount is read by the per-pair and
// per-entry gaming detectors, the ring detector, and the pool_royalty_margin /
// distill_royalty_margin views (margin_usd = avoided_cogs_usd − minted_amount) — so
// whether it means "royalty the serve was worth" or "LENS actually minted" changes
// reported margin and every detector threshold calibrated against it. So this pins the
// measured behaviour instead, and DELIBERATELY EXPIRES: the moment the claim row records
// the effective amount, TestBondedMintSettlement_KnownHole goes RED and says so.
//
// The population arm asks the second question the pin cannot: WHICH of the sweeper's six
// mints are exposed. It observes the real kernel (mint, then read held_balance) rather
// than asking isReputationBondedType — the set whose completeness is in question is never
// its own witness.

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/mining"
)

// bondSettleR is a mid-ramp reputation: f(R) = (0.425−0.35)/(0.50−0.35) = 0.5 EXACTLY, so a
// bonded mint halves and a non-bonded one does not — an unambiguous, rounding-free signal.
// NOT below AccessFloor: a below-floor mint is REFUSED, which is the working path and would
// hide the defect behind a clean rollback.
const bondSettleR = 0.425

// bondSettleExposed is the DECLARED set of sweeper claim tables whose mint is reduced by the
// reputation bond and therefore cannot settle its own claim row. Declared per table, never
// derived from the allow-list under test.
//
// pool_royalty_mints and distill_royalty_mints are both here because both mint
// TypePoolRoyaltyHeld — the distill minter deliberately reuses the Pool-B held kernel
// (distill_minter.go), so it inherits the bond and the divergence with it.
var bondSettleExposed = map[string]bool{
	"pool_royalty_mints":         true,
	"distill_royalty_mints":      true,
	"eval_contribution_mints":    false,
	"routing_prediction_mints":   false,
	"node_latency_mints":         false,
	"confidential_compute_mints": false,
}

// bondSettleHarness builds the real-PG harness with the P1 #9 flag ARMED — the state the
// defect needs. bondSettleHarnessGate(t, false) is the SAME harness with the flag off, which
// is production's default and the control that proves the flag is the only difference.
func bondSettleHarness(t *testing.T) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	return bondSettleHarnessGate(t, true)
}

func bondSettleHarnessGate(t *testing.T, armed bool) (*pgxpool.Pool, *mining.LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG bonded-settlement test")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	// Private schema (the Lens gated-test convention) so a parallel package's DROP/CREATE of
	// the same table names cannot collide on the shared DB.
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_bondsettle_test"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddl := []string{
		`DROP SCHEMA IF EXISTS lens_bondsettle_test CASCADE`,
		`CREATE SCHEMA lens_bondsettle_test`,
		`CREATE TABLE lens_token_balances (workspace_id TEXT PRIMARY KEY, balance BIGINT NOT NULL DEFAULT 0,
			held_balance BIGINT NOT NULL DEFAULT 0, lifetime_earned BIGINT NOT NULL DEFAULT 0,
			lifetime_spent BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE lens_token_ledger (id UUID NOT NULL DEFAULT gen_random_uuid(), workspace_id TEXT NOT NULL,
			amount BIGINT NOT NULL, balance_after BIGINT NOT NULL, type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '', metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (id, workspace_id))`,
		`CREATE TABLE reputation_events (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), annotator_id TEXT NOT NULL,
			kind TEXT NOT NULL, idem_key TEXT NOT NULL, delta DOUBLE PRECISION NOT NULL,
			reason JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (annotator_id, kind, idem_key))`,
		`CREATE INDEX idx_reputation_events_annotator ON reputation_events (annotator_id)`,
		// The real pool-royalty claim table, as the real minter writes it.
		`CREATE TABLE pool_royalty_mints (id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id TEXT NOT NULL UNIQUE, requester_workspace_id TEXT NOT NULL,
			contributor_workspace_id TEXT NOT NULL, layer TEXT NOT NULL, entry_id TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			similarity DOUBLE PRECISION NOT NULL DEFAULT 0, avoided_cogs_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
			minted_amount BIGINT NOT NULL DEFAULT 0, answer_sha256 TEXT NOT NULL DEFAULT '',
			prompt_sha256 TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'final',
			finalize_after TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
	}
	for _, stmt := range ddl {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := mining.NewLedgerStore(pool)
	ledger.SetReputationGate(func() bool { return armed }) // P1 #9 flag — ARMED is the whole point
	return pool, ledger
}

// seedBondR sets a workspace's reputation to `target` (reputation.go reads baseline + Σdelta).
func seedBondR(t *testing.T, pool *pgxpool.Pool, ws string, target float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO reputation_events (annotator_id, kind, idem_key, delta) VALUES ($1,'seed','seed',$2)`,
		ws, target-0.5); err != nil {
		t.Fatalf("seedBondR: %v", err)
	}
}

func bondHeldOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var held int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT held_balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&held)
	return held
}

func bondBalanceOf(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var bal int64
	_ = pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&bal)
	return bal
}

// TestBondedMintSettlement_KnownHole drives the REAL pool-royalty minter and the REAL
// finalize sweeper, end to end, with the reputation bond armed — no hand-written claim row,
// no hand-written held credit. Every number below is read back out of Postgres.
//
// ⚠ THIS PIN EXPIRES BY DESIGN. It asserts a BROKEN behaviour because the repair is a
// money-path decision (see the file header). Each assertion carries the exact message to
// act on when it flips: the day a bonded claim row records what was actually held, this
// test goes red and must be REPLACED by the invariant it currently documents —
// claim.minted_amount == held credited, and a due bonded mint settles.
func TestBondedMintSettlement_KnownHole(t *testing.T) {
	pool, ledger := bondSettleHarness(t)
	ctx := context.Background()
	const ws = "ws_bonded_contributor"
	seedBondR(t, pool, ws, bondSettleR)

	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	if res, err := m.MintServedHit(ctx, ServedHit{
		RequestID: "client-header", RequesterWorkspace: "ws_requester", ContributorWorkspace: ws,
		Layer: "exact", EntryID: "entry-1", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0,
		AnswerSHA256:   SHA256Hex([]byte("answer")), PromptSHA256: SHA256Hex([]byte("prompt")),
	}); err != nil || !res.Minted {
		t.Fatalf("the bonded mint itself must succeed (it is only REDUCED, not refused): res=%+v err=%v", res, err)
	}

	base := micro(0.5 * 2.0 * economy.LENSPerUSD) // share × avoided COGS × peg, before the bond
	held := bondHeldOf(t, pool, ws)
	var claimed int64
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT minted_amount, status FROM pool_royalty_mints WHERE contributor_workspace_id=$1`, ws).
		Scan(&claimed, &status); err != nil {
		t.Fatal(err)
	}

	// (1) The bond really did reduce the held credit — observed in the balances table, not
	// assumed from f(R). If this fails the bond is inert and the rest of the test is vacuous.
	if held != base/2 {
		t.Fatalf("held_balance=%d, want %d (base %d halved by f(%.3f)=0.5) — the reputation bond did NOT "+
			"reduce this mint, so every assertion below would be measuring nothing",
			held, base/2, base, bondSettleR)
	}
	// (2) THE HOLE: the sweeper's input still carries the UNREDUCED base.
	if claimed != base {
		t.Fatalf("PIN EXPIRED (and this is the good direction): claim minted_amount=%d, held=%d. "+
			"The claim row no longer carries the unreduced base — the divergence this file documents is "+
			"GONE. DELETE this test and assert the invariant instead: minted_amount == held credited, for "+
			"every bonded mint, and a due bonded mint settles on the first sweep.", claimed, held)
	}
	if claimed == held {
		t.Fatalf("unreachable given base/2 != base; guard against a future base of 0")
	}

	// (3) THE CONSEQUENCE, through the real sweeper: the mint cannot settle. RunOnce reports
	// no error — the failure is WARN-logged and swallowed, so nothing upstream can see it.
	if _, err := pool.Exec(ctx,
		`UPDATE pool_royalty_mints SET finalize_after = now() - interval '1 second'`); err != nil {
		t.Fatal(err) // age the row: the only thing the production clock contributes
	}
	n, err := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints").RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error=%v — measured behaviour is a swallowed per-row failure, not a returned "+
			"error; if that changed, the finding's 'invisible to the caller' half is out of date", err)
	}
	if n != 0 {
		t.Fatalf("PIN EXPIRED (and this is the good direction): the sweeper settled %d bonded rows. A "+
			"bonded mint now settles — DELETE this test and assert the invariant directly.", n)
	}
	if got := bondBalanceOf(t, pool, ws); got != 0 {
		t.Fatalf("spendable balance=%d, want 0 — nothing settled, so nothing may have become spendable", got)
	}
	if got := bondHeldOf(t, pool, ws); got != base/2 {
		t.Fatalf("held_balance=%d after the failed sweep, want %d unchanged — a failed finalize must roll "+
			"back cleanly, leaving the contributor's held LENS intact", got, base/2)
	}
	var status2 string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM pool_royalty_mints WHERE contributor_workspace_id=$1`, ws).Scan(&status2); err != nil {
		t.Fatal(err)
	}
	if status2 != "held" {
		t.Fatalf("claim status=%q after the failed sweep, want \"held\" — the CAS must roll back with the "+
			"finalize, or the row would be marked settled while no LENS moved", status2)
	}
}

// TestBondedMintSettlement_UnbondedSettles is the MUST-STAY-GREEN companion, and it is what
// keeps TestBondedMintSettlement_KnownHole from being a test that would pass over a healthy
// system. It runs the SAME mint, the SAME contributor, the SAME mid-ramp reputation and the
// SAME sweeper with ONE thing changed — the P1 #9 flag off, which is production's default.
//
// It pins two claims the finding rests on, neither of which the KnownHole case can state:
//
//   - the flag is the whole difference. Reputation R=0.425 is seeded here too, so if this
//     settles and KnownHole does not, the reducer is the cause and not the reputation read,
//     the schema, the holdback clock or the sweeper.
//   - "latent, not live". The comments merged onto config.go and minter.go say a bonded mint
//     is one env var away from being stuck, which is only honest if the flag being OFF really
//     does settle. Measured here rather than asserted there.
//
// It also fixes the direction of the KnownHole assertions: n==0 there means something, because
// n==1 is reachable at all.
func TestBondedMintSettlement_UnbondedSettles(t *testing.T) {
	pool, ledger := bondSettleHarnessGate(t, false) // the ONE difference from KnownHole
	ctx := context.Background()
	const ws = "ws_unbonded_contributor"
	seedBondR(t, pool, ws, bondSettleR) // same mid-ramp R — so R is not the variable

	m := NewMinter(pool, ledger, 0.5, func() bool { return true })
	if res, err := m.MintServedHit(ctx, ServedHit{
		RequestID: "client-header", RequesterWorkspace: "ws_requester", ContributorWorkspace: ws,
		Layer: "exact", EntryID: "entry-1", Provider: "openai", Model: "gpt-4o",
		AvoidedCOGSUSD: 2.0,
		AnswerSHA256:   SHA256Hex([]byte("answer")), PromptSHA256: SHA256Hex([]byte("prompt")),
	}); err != nil || !res.Minted {
		t.Fatalf("the unbonded mint must succeed: res=%+v err=%v", res, err)
	}

	base := micro(0.5 * 2.0 * economy.LENSPerUSD)
	var claimed int64
	if err := pool.QueryRow(ctx,
		`SELECT minted_amount FROM pool_royalty_mints WHERE contributor_workspace_id=$1`, ws).
		Scan(&claimed); err != nil {
		t.Fatal(err)
	}
	// With the flag off the two records of the mint AGREE — which is the invariant the bonded
	// path breaks, stated here in the one configuration where it currently holds.
	if held := bondHeldOf(t, pool, ws); held != base || claimed != base {
		t.Fatalf("held=%d claim.minted_amount=%d, want both %d — with the reputation bond OFF the "+
			"claim row and the held credit must be the same number; if they already differ here, the "+
			"divergence is NOT the flag and the finding's 'latent, not live' half is wrong", held, claimed, base)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE pool_royalty_mints SET finalize_after = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	n, err := NewFinalizeSweeper(pool, ledger, "pool_royalty_mints").RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce error=%v", err)
	}
	if n != 1 {
		t.Fatalf("settled %d rows, want 1 — an UNBONDED due mint must settle on the first sweep. If "+
			"this fails, KnownHole's n==0 proves nothing: the sweeper is broken for every mint, not "+
			"only bonded ones, and the finding is misattributed", n)
	}
	if got := bondBalanceOf(t, pool, ws); got != base {
		t.Fatalf("spendable balance=%d, want %d — a settled mint must be spendable in full", got, base)
	}
	if got := bondHeldOf(t, pool, ws); got != 0 {
		t.Fatalf("held_balance=%d, want 0 — settling moves held → spendable, it does not duplicate", got)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM pool_royalty_mints WHERE contributor_workspace_id=$1`, ws).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "final" {
		t.Fatalf("claim status=%q, want \"final\"", status)
	}
}

// TestBondedMintSettlement_ExposedPopulation answers WHICH of the sweeper's mints are
// exposed, and does it by OBSERVING the held kernel rather than by reading the allow-list
// whose completeness is the question. For each claim table the sweeper settles it mints the
// table's held type through the real CreditHeldTx with the bond armed and compares the
// resulting held_balance to the base.
//
// Reds three ways, all of them things somebody needs to see:
//   - a NEW claim table joins the sweeper and nobody declared whether its mint is bonded;
//   - a declared-unexposed mint becomes bonded (it silently joins the stuck-row population);
//   - a declared-exposed mint stops being bonded (the hole is closing — update the pin).
func TestBondedMintSettlement_ExposedPopulation(t *testing.T) {
	pool, ledger := bondSettleHarness(t)
	ctx := context.Background()
	const base int64 = 10_000_000

	tables := make([]string, 0, len(finalTypeForTable))
	for tbl := range finalTypeForTable {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)

	for _, tbl := range tables {
		declared, ok := bondSettleExposed[tbl]
		if !ok {
			t.Errorf("UNDECLARED SWEEPER TABLE %q: the finalize sweeper settles it, but nobody declared "+
				"whether its mint is reduced by the reputation bond. If it is, its claim row and its held "+
				"credit disagree the moment the bond is armed and it can never settle — decide, then add it "+
				"to bondSettleExposed.", tbl)
			continue
		}
		// The mint's HELD type, from the sweeper's own held↔final pairing (final + "_held"),
		// which TestSweeperFinalizedType_PairsWithHeldType pins independently.
		heldType := finalTypeForTable[tbl] + "_held"
		ws := "ws_pop_" + strings.TrimSuffix(tbl, "_mints")
		seedBondR(t, pool, ws, bondSettleR)

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := ledger.CreditHeldTx(ctx, tx, ws, base, heldType, "population probe", nil); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: held mint of %q failed: %v", tbl, heldType, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		got := bondHeldOf(t, pool, ws)
		exposed := got != base // the kernel reduced it ⇒ the claim row will not match
		switch {
		case exposed && got != base/2:
			t.Errorf("%s (%s): held=%d — reduced, but not to the f(%.3f)=0.5 half (%d) this test can "+
				"reason about. The reducer changed; re-derive the expected factor before trusting the rest.",
				tbl, heldType, got, bondSettleR, base/2)
		case exposed && !declared:
			t.Errorf("%s (%s): NEWLY EXPOSED — the bond reduced this mint to %d of %d, so its claim row "+
				"(minted_amount=%d) can no longer be settled out of its own held balance. It has joined the "+
				"permanently-stuck population. Declared unexposed in bondSettleExposed.",
				tbl, heldType, got, base, base)
		case !exposed && declared:
			t.Errorf("%s (%s): NO LONGER EXPOSED — held=%d equals the base, so the bond no longer reduces "+
				"this mint. Either the hole is closing (good — update bondSettleExposed and see whether "+
				"TestBondedMintSettlement_KnownHole should be deleted) or the bond went inert (bad).",
				tbl, heldType, got)
		}
	}
}
