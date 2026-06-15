package forecast

import (
	"context"

	"github.com/talyvor/lens/internal/budgets"
)

// U8/U9 SEAM LOCK. forecast.Store holds the read-replica pool in production
// (main.go wires forecast.NewStore via dbrouting.ReadPool). It is safe there
// ONLY because the budgets dependency is reached through the read-only
// budgetSpend interface — forecast can never write through it. If budgetSpend
// is later widened with a write (e.g. an Upsert/Exec), forecast would silently
// become a writer holding the replica pool — the exact entanglement U8/U9
// excludes.
//
// budgetSpendReadOnly enumerates EXACTLY the methods budgetSpend is allowed to
// expose (both READS). The two compile-time assertions below pin budgetSpend's
// method set to this shape in BOTH directions, so the set cannot grow OR shrink
// without breaking the build:
//   - budgetSpend ⊆ budgetSpendReadOnly  → no method may be ADDED (a write trips it)
//   - budgetSpendReadOnly ⊆ budgetSpend  → both reads must remain
type budgetSpendReadOnly interface {
	ReconcileSpent(ctx context.Context, b budgets.Budget) (float64, error)
	List(ctx context.Context, workspaceID string) ([]budgets.Budget, error)
}

var (
	// budgetSpend implements budgetSpendReadOnly → budgetSpend has no method
	// beyond the two allowed reads. Adding a write to budgetSpend breaks THIS.
	_ budgetSpendReadOnly = (budgetSpend)(nil)
	// budgetSpendReadOnly implements budgetSpend → the seam still has both reads.
	_ budgetSpend = (budgetSpendReadOnly)(nil)
)
