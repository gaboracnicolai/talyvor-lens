// Package earnverify implements the U6 verified-to-earn predicate (the Sybil
// floor): a workspace may MINT / accrue royalty only when it is verified.
//
// "Verified to earn" = an admin-set earn_verified override (the enterprise /
// self-host vouch) OR a completed real-money lxc_purchase (refunded / anomalous
// deliberately excluded — closes the buy→refund→stay-verified loop). The
// completed-purchase half is derived at READ time, so the money-path billing
// webhook is never written by this gate and there is no write race.
package earnverify

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Verifier satisfies mining.MintVerifier. It is stateless: MayEarn reads on the
// MINT tx it is handed, so the check is consistent with the credit it gates and
// needs no pool of its own.
type Verifier struct {
	// requireLive tightens the purchase half to REAL-money purchases only. Env:
	// LENS_EARN_REQUIRE_LIVE_PURCHASE, default FALSE.
	//
	// Default false is what lets a TEST-key trial work: a test purchase is recorded
	// with livemode=false and still verifies, so trial users become earn-verified by
	// buying rather than by a manual UPDATE. Flipping it true closes the door — test
	// purchases stop verifying — WITHOUT a code change or a migration, which is the
	// point: the trial works now and the door shuts before open signup.
	requireLive bool
	// testWorkspaces verifies every test (synthetic) workspace — see WithTestWorkspaces.
	testWorkspaces bool
}

// New builds the verified-to-earn verifier. Wire it UNCONDITIONALLY at startup
// via LedgerStore.SetMintVerifier — a safety restriction must not be liftable by
// the economy toggle.
func New(requireLive bool) Verifier { return Verifier{requireLive: requireLive} }

// WithTestWorkspaces also verifies every test (synthetic) workspace, whatever requireLive says (B26.6): its
// starting grant is test-backed money, and what it earns is test money — marked test (0173) and never paid
// out (B25.1). Production's mint verifier is built with it; a real workspace is judged exactly as before.
func (v Verifier) WithTestWorkspaces() Verifier {
	v.testWorkspaces = true
	return v
}

// mayEarnSQL. The purchase half additionally requires that the purchase's MODE was
// RECORDED (livemode IS NOT NULL) and, when $2 is true, that it was REAL money
// (livemode). $2 is LENS_EARN_REQUIRE_LIVE_PURCHASE — see Verifier.
//
// A legacy row from before migration 0109 has livemode NULL and therefore stops
// conferring earning rights: the column exists precisely because a row that does
// not say which mode produced it cannot be trusted to mean real money.
const mayEarnSQL = `SELECT
	EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND earn_verified = true)
	OR EXISTS(SELECT 1 FROM lxc_purchases
	          WHERE workspace_id = $1 AND status = 'completed' AND lxc_amount > 0
	            AND livemode IS NOT NULL
	            AND (NOT $2::boolean OR livemode))`

// MayEarn reports whether wsID is verified-to-earn. Read at mint time on the
// mint tx. An empty workspace_id is never verified.
func (v Verifier) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	if workspaceID == "" {
		return false, nil
	}
	var ok bool
	if v.testWorkspaces {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT synthetic FROM workspaces WHERE id = $1), false)`,
			workspaceID).Scan(&ok); err != nil || ok {
			return ok, err
		}
	}
	if err := tx.QueryRow(ctx, mayEarnSQL, workspaceID, v.requireLive).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
