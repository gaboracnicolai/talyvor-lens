package mining

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// P1 #9 reputation-bonded minting — chokepoint proof (real PG). A bonded mint (receipt /
// pool_royalty_held) is scaled by f(R) and gated to 0 below the access floor, as an ADDITIVE
// constraint DOWNSTREAM of the U6 verified-floor — it can only reduce or block, never enable. Wired
// like prod: SetMintVerifier (U6) + SetReputationGate (the flag).

// repFakeVerifier is a minimal U6 MintVerifier — verified[ws]==true ⇒ MayEarn true. Lets us prove
// compose-not-bypass without the earnverify package (avoids any import question).
type repFakeVerifier struct{ verified map[string]bool }

func (f repFakeVerifier) MayEarn(_ context.Context, _ pgx.Tx, ws string) (bool, error) {
	return f.verified[ws], nil
}

func repBondHarness(t *testing.T, gateOn bool, verified ...string) (*pgxpool.Pool, *LedgerStore) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG reputation-bond test")
	}
	// Private schema (the Lens gated-test convention) so a parallel package's DROP/CREATE of the same
	// table names can't collide on the shared DB (-p 2 runs mining + povi concurrently).
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "lens_repbond_test"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS lens_repbond_test CASCADE`,
		`CREATE SCHEMA lens_repbond_test`,
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
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ledger := NewLedgerStore(pool)
	v := repFakeVerifier{verified: map[string]bool{}}
	for _, ws := range verified {
		v.verified[ws] = true
	}
	ledger.SetMintVerifier(v)                               // U6 floor, like prod
	ledger.SetReputationGate(func() bool { return gateOn }) // P1 #9 flag
	return pool, ledger
}

// seedR sets a workspace's reputation to ~target by appending one event with delta = target − 0.5.
func seedR(t *testing.T, pool *pgxpool.Pool, ws string, target float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO reputation_events (annotator_id, kind, idem_key, delta) VALUES ($1,'seed','seed',$2)`,
		ws, target-0.5); err != nil {
		t.Fatalf("seedR: %v", err)
	}
}

func bal(t *testing.T, pool *pgxpool.Pool, ws string) int64 {
	t.Helper()
	var b int64
	_ = pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT balance FROM lens_token_balances WHERE workspace_id=$1),0)`, ws).Scan(&b)
	return b
}

func ledgerRows(t *testing.T, pool *pgxpool.Pool, ws string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM lens_token_ledger WHERE workspace_id=$1`, ws).Scan(&n)
	return n
}

const rcptType = "receipt_mine_provisional" // bonded type

// (1) High-R (≥0.5) → full base. (2) Below-floor (<0.35) → ErrReputationFloor, no row, balance unchanged.
func TestRepBond_HighFull_BelowFloorGated_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsHigh", "wsLow")
	ctx := context.Background()

	seedR(t, pool, "wsHigh", 0.7) // ≥ baseline → f=1.0
	if err := ledger.Credit(ctx, "wsHigh", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("high-R mint: %v", err)
	}
	if b := bal(t, pool, "wsHigh"); b != micro(10) {
		t.Errorf("high-R balance %v, want 10 (full base)", b)
	}

	seedR(t, pool, "wsLow", 0.30) // < floor 0.35 → gated
	err := ledger.Credit(ctx, "wsLow", micro(10), rcptType, "d", map[string]interface{}{})
	if err != ErrReputationFloor {
		t.Fatalf("below-floor mint err = %v, want ErrReputationFloor", err)
	}
	if b := bal(t, pool, "wsLow"); b != 0 {
		t.Errorf("below-floor balance %v, want 0 (rolled back)", b)
	}
	if n := ledgerRows(t, pool, "wsLow"); n != 0 {
		t.Errorf("below-floor ledger rows %d, want 0", n)
	}
}

// (3) Mid-ramp R=0.425 → effective == base·0.5 exactly; metadata {base,reputation,effective} present.
func TestRepBond_MidRampHalf_Metadata_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsMid")
	ctx := context.Background()
	seedR(t, pool, "wsMid", 0.425) // f = (0.425−0.35)/0.15 = 0.5

	if err := ledger.Credit(ctx, "wsMid", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("mid mint: %v", err)
	}
	if b := bal(t, pool, "wsMid"); b != micro(5) {
		t.Errorf("mid-ramp balance %v, want 5 (base 10 × f 0.5)", b)
	}
	// SEC-2: reputation_base/effective are µLENS integers now (keys carry _ulens).
	var base, eff *int64
	var score *float64
	if err := pool.QueryRow(ctx, `SELECT (metadata->>'reputation_base_ulens')::bigint, (metadata->>'reputation_score')::float8,
		(metadata->>'reputation_effective_ulens')::bigint FROM lens_token_ledger WHERE workspace_id='wsMid'`).Scan(&base, &score, &eff); err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	if base == nil || score == nil || eff == nil || *base != micro(10) || *eff != micro(5) {
		t.Errorf("metadata {base,score,eff} = {%v,%v,%v} µLENS, want base micro(10) / eff micro(5) / score≈0.425", base, score, eff)
	}
}

// (4) NO-LOOP — minting N times moves NEITHER the reputation_events count NOR R.
func TestRepBond_NoLoop_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsLoop")
	ctx := context.Background()
	seedR(t, pool, "wsLoop", 0.7)

	countEvents := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM reputation_events WHERE annotator_id='wsLoop'`).Scan(&n)
		return n
	}
	before := countEvents()
	for i := 0; i < 25; i++ {
		if err := ledger.Credit(ctx, "wsLoop", 1, rcptType, "d", map[string]interface{}{}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if after := countEvents(); after != before {
		t.Errorf("NO-LOOP violated: reputation_events %d→%d — minting must NEVER write a reputation event", before, after)
	}
	// R is still a pure function of the (unchanged) events → unchanged.
	var sum float64
	_ = pool.QueryRow(ctx, `SELECT COALESCE(SUM(delta),0) FROM reputation_events WHERE annotator_id='wsLoop'`).Scan(&sum)
	if clampReputation(ReputationBaseline+sum) != 0.7 {
		t.Errorf("R moved to %v after minting — must stay 0.7", clampReputation(ReputationBaseline+sum))
	}
}

// (6) Flag-OFF → byte-identical: a below-floor workspace mints the FULL base (no read, no gate).
func TestRepBond_FlagOffByteIdentical_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, false, "wsOff") // gate OFF
	ctx := context.Background()
	seedR(t, pool, "wsOff", 0.10) // far below floor — would be gated if the flag were on

	if err := ledger.Credit(ctx, "wsOff", micro(10), rcptType, "d", map[string]interface{}{}); err != nil {
		t.Fatalf("flag-off mint: %v", err)
	}
	if b := bal(t, pool, "wsOff"); b != micro(10) {
		t.Errorf("flag-off balance %v, want 10 (full base — reputation not consulted)", b)
	}
}

// (7) Composes-not-bypasses U6: unverified + R=1.0 → still ErrEarnNotVerified (floor wins);
// verified + R<floor → still ErrReputationFloor (reputation can block a U6-allowed mint).
func TestRepBond_ComposesNotBypassesU6_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsVerified") // only wsVerified passes U6
	ctx := context.Background()
	seedR(t, pool, "wsUnverified", 1.0) // max reputation, but NOT verified-to-earn
	seedR(t, pool, "wsVerified", 0.30)  // verified, but below the reputation floor

	if err := ledger.Credit(ctx, "wsUnverified", micro(10), rcptType, "d", map[string]interface{}{}); err != ErrEarnNotVerified {
		t.Fatalf("unverified+R=1.0 err = %v, want ErrEarnNotVerified (U6 floor wins; reputation never enables)", err)
	}
	if err := ledger.Credit(ctx, "wsVerified", micro(10), rcptType, "d", map[string]interface{}{}); err != ErrReputationFloor {
		t.Fatalf("verified+R<floor err = %v, want ErrReputationFloor (reputation blocks a U6-allowed mint)", err)
	}
}

// Held path (pool_royalty_held) is bonded too: a below-floor workspace's held mint is gated.
func TestRepBond_HeldPathBonded_Integration(t *testing.T) {
	pool, ledger := repBondHarness(t, true, "wsHeld")
	ctx := context.Background()
	seedR(t, pool, "wsHeld", 0.30)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ledger.CreditHeldTx(ctx, tx, "wsHeld", 10, TypePoolRoyaltyHeld, "d", map[string]interface{}{}); err != ErrReputationFloor {
		t.Fatalf("held below-floor err = %v, want ErrReputationFloor", err)
	}
}

// Unit: f(R) is bounded, monotone, =0 below floor, =1 at/above baseline, never >1.
func TestReputationFactor_Bounded(t *testing.T) {
	cases := []struct {
		r, want float64
	}{
		{0.0, 0}, {0.34, 0}, {0.35, 0}, {0.425, 0.5}, {0.50, 1}, {0.75, 1}, {1.0, 1},
	}
	for _, c := range cases {
		got := reputationFactor(c.r)
		if got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("reputationFactor(%v) = %v, want %v", c.r, got, c.want)
		}
		if got > 1.0 {
			t.Errorf("reputationFactor(%v) = %v > 1.0 — must never amplify", c.r, got)
		}
	}
}
