package proxy

// B26.6 — TEST USERS EARN THEIR ROYALTIES (real PG, real migrated schema).
//
// A pooled serve between two test (synthetic) workspaces is paid for with the consumer's starting grant,
// which counts as test-backed money: it funds the contributor's royalty, marked test (0173), with
// LENS_EARN_REQUIRE_LIVE_PURCHASE on. The same serve between two REAL workspaces funded the same way mints
// exactly as before — nothing, because no cash paid for it. Every assertion is a ledger row.

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/earnverify"
	"github.com/talyvor/lens/internal/mining"
)

func TestTestUsersEarnPooledRoyalty_B266_Integration(t *testing.T) {
	ctx := context.Background()

	// Two fresh test users as the synthetic route makes them: synthetic, never earn-verified, no purchase,
	// funded only by the 1,000 LXC starting grant.
	e := newUnbackedEnv(t, "b266-test")
	e.lens.SetMintVerifier(earnverify.New(true).WithTestWorkspaces()) // as main.go wires it, LENS_EARN_REQUIRE_LIVE_PURCHASE=true
	e.p.SetSyntheticLookup(func(ws string) bool { return ws == e.buyer || ws == e.seller })
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET synthetic = true, earn_verified = false WHERE id = ANY($1)`,
		[]string{e.buyer, e.seller}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.GrantLXC(ctx, e.buyer, 1_000_000_000, "synthetic test credits", map[string]interface{}{"synthetic": true}); err != nil {
		t.Fatal(err)
	}

	funded := e.serveCrossTenantPooledHit(t)
	var n, ln int
	var minted, lamt int64
	var mintTest, ledgerTest bool
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(minted_amount), 0), COALESCE(bool_and(test), false)
		FROM pool_royalty_mints WHERE requester_workspace_id = $1 AND contributor_workspace_id = $2`,
		e.buyer, e.seller).Scan(&n, &minted, &mintTest); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(amount), 0), COALESCE(bool_and(test), false)
		FROM lens_token_ledger WHERE workspace_id = $1 AND type = $2`,
		e.seller, mining.TypePoolRoyaltyHeld).Scan(&ln, &lamt, &ledgerTest); err != nil {
		t.Fatal(err)
	}
	t.Logf("TEST PAIR: funded=$%.6f  pool_royalty_mints rows=%d minted=%d test=%v  lens_token_ledger rows=%d amount=%d test=%v",
		funded, n, minted, mintTest, ln, lamt, ledgerTest)
	if funded <= 0 {
		t.Fatalf("a test consumer's charge funded $%v — its test-backed grant must fund the royalty", funded)
	}
	if n != 1 || minted <= 0 || !mintTest {
		t.Fatalf("pool_royalty_mints: rows=%d minted=%d test=%v, want one test-marked royalty", n, minted, mintTest)
	}
	if ln != 1 || lamt != minted || !ledgerTest {
		t.Fatalf("contributor's %s rows=%d amount=%d test=%v, want one test-marked row of %d",
			mining.TypePoolRoyaltyHeld, ln, lamt, ledgerTest, minted)
	}

	// The same serve between two real workspaces funded by the same grant: unchanged, nothing mints.
	r := newUnbackedEnv(t, "b266-real")
	r.lens.SetMintVerifier(earnverify.New(true).WithTestWorkspaces())
	if _, err := r.store.GrantLXC(ctx, r.buyer, 1_000_000_000, "onboarding comp", nil); err != nil {
		t.Fatal(err)
	}
	rFunded := r.serveCrossTenantPooledHit(t)
	rn, rMinted := r.mintRows(t)
	rln, rlamt := r.royaltyLedgerRows(t)
	t.Logf("REAL PAIR: funded=$%.6f  pool_royalty_mints rows=%d minted=%d  lens_token_ledger rows=%d amount=%d",
		rFunded, rn, rMinted, rln, rlamt)
	if rFunded != 0 || rn != 0 || rMinted != 0 || rln != 0 || rlamt != 0 {
		t.Fatalf("a real workspace's granted credit funded $%v and minted %d µLENS (%d claim rows, %d ledger rows) — real grants must still fund nothing",
			rFunded, rMinted, rn, rln)
	}
}
