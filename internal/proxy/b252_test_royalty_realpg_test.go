package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// B25.2 — A POOLED SERVE BETWEEN TWO TEST USERS PAYS THE CONTRIBUTOR'S ROYALTY, MARKED TEST.
//
// The real migrated schema (0173's test mark), wired as production wires it: the proxy knows which workspaces
// are synthetic from workspaces.synthetic, and the minter has the money wall. Both workspaces are test users;
// the asker paid for its credits with a Stripe test card (the credit billing writes). The SAME seam production
// runs — hold, settle the pooled charge, mint — and every assertion is on the earnings rows.
func TestB252_APooledServeBetweenTwoTestUsersPaysTheContributorsRoyaltyMarkedTest(t *testing.T) {
	e := newUnbackedEnv(t, "b252-test")
	ctx := context.Background()
	for _, ws := range []string{e.buyer, e.seller} {
		if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET synthetic = true WHERE id = $1`, ws); err != nil {
			t.Fatal(err)
		}
	}
	e.p.SetSyntheticLookup(func(ws string) bool {
		var s bool
		_ = e.pool.QueryRow(ctx, `SELECT synthetic FROM workspaces WHERE id = $1`, ws).Scan(&s)
		return s
	})
	e.p.royaltyMinter.(*poolroyalty.Minter).SetMoneyWall(workspace.CheckMoneyWall)

	// The asker's test-card top-up, credited exactly as internal/billing credits a completed test-mode session.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreditLXCTx(ctx, tx, e.buyer, 500_000_000, "stripe top-up", map[string]interface{}{"funding": "test"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if funded := e.serveCrossTenantPooledHit(t); funded <= 0 {
		t.Fatalf("the test user's pooled serve settled $%v — nothing was charged, so nothing can be earned", funded)
	}
	var claims int
	var minted int64
	var claimTest bool
	if err := e.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(minted_amount), 0), COALESCE(bool_and(test), false)
		FROM pool_royalty_mints WHERE requester_workspace_id = $1 AND contributor_workspace_id = $2`, e.buyer, e.seller).
		Scan(&claims, &minted, &claimTest); err != nil {
		t.Fatal(err)
	}
	var earned int64
	var earnedTest bool
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0), COALESCE(bool_and(test), false) FROM lens_token_ledger
		WHERE workspace_id = $1 AND type = $2`, e.seller, mining.TypePoolRoyaltyHeld).Scan(&earned, &earnedTest); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || minted <= 0 || earned != minted || !claimTest || !earnedTest {
		t.Fatalf("contributor's royalty: %d claim(s) minting %d µLENS (test=%v), %d µLENS on its ledger (test=%v); "+
			"want one claim, the same amount earned, both marked test", claims, minted, claimTest, earned, earnedTest)
	}
}
