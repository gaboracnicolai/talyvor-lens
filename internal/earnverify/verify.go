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
type Verifier struct{}

// New builds the verified-to-earn verifier. Wire it UNCONDITIONALLY at startup
// via LedgerStore.SetMintVerifier — a safety restriction must not be liftable by
// the economy toggle.
func New() Verifier { return Verifier{} }

const mayEarnSQL = `SELECT
	EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND earn_verified = true)
	OR EXISTS(SELECT 1 FROM lxc_purchases WHERE workspace_id = $1 AND status = 'completed' AND lxc_amount > 0)`

// MayEarn reports whether wsID is verified-to-earn. Read at mint time on the
// mint tx. An empty workspace_id is never verified.
func (Verifier) MayEarn(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	if workspaceID == "" {
		return false, nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, mayEarnSQL, workspaceID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
