// Package royaltyhaircut is the KE-2 drift-oracle: it computes a REDUCE-ONLY royalty multiplier for a
// workspace from Keel's HARDENED idiosyncratic drift findings. It is MINT-FREE — it reads keel_findings via a
// caller-supplied tx and imports no ledger/economy package; the money-path BOUND (the floor, the clamp, the
// fail-open) lives in internal/mining (HaircutFloor), so a bug here can only ever be a smaller reduction, never
// a burn and never below the floor.
//
// FAIL-OPEN BY CONSTRUCTION (the H5 lesson — a false penalty is worse than an open flank):
//   - ONLY mode='hardened' findings count. Hardened is ALWAYS idiosyncratic by construction (a common-mode,
//     cohort-wide shift moves the OTHERS too, so a leave-one-out drop cannot arise from it — keel_hardened
//     never emits common_mode). The attribution='idiosyncratic' predicate is belt-and-suspenders and
//     SQL-enforces that a common_mode row can NEVER trigger a haircut.
//   - Ordinary (contaminable-mean) findings NEVER count.
//   - Only findings at/after minWindowBucket count (a long-ago drift does not penalise forever).
package royaltyhaircut

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DriftFactor is the target reduced fraction applied when a sustained hardened idiosyncratic drift is present:
// a drifting contributor earns this fraction of its bonded royalty. PLACEHOLDER — calibrate at N3. The mining
// money-path re-clamps to [mining.HaircutFloor, 1.0] regardless, so this value can never zero a contributor.
const DriftFactor = 0.5

// hardenedFindingSQL asks: does workspaceID have a CURRENT hardened idiosyncratic drift finding? The
// mode='hardened' + attribution='idiosyncratic' predicates SQL-enforce the fail-open rule.
const hardenedFindingSQL = `SELECT EXISTS (
    SELECT 1 FROM keel_findings
    WHERE workspace_id = $1
      AND mode = 'hardened'
      AND attribution = 'idiosyncratic'
      AND window_bucket >= $2)`

// Factor returns the reduce-only royalty multiplier for workspaceID: DriftFactor when a current hardened
// idiosyncratic drift finding exists (window_bucket >= minWindowBucket), else 1.0 (no haircut). It NEVER
// returns > 1.0 (cannot increase a mint). On a read error it returns 1.0 AND the error — the caller (mining)
// fails open (no haircut) and logs.
func Factor(ctx context.Context, tx pgx.Tx, workspaceID string, minWindowBucket int64) (float64, error) {
	var drifting bool
	if err := tx.QueryRow(ctx, hardenedFindingSQL, workspaceID, minWindowBucket).Scan(&drifting); err != nil {
		return 1.0, err
	}
	if drifting {
		return DriftFactor, nil
	}
	return 1.0, nil
}
