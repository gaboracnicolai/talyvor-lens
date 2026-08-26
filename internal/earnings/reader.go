package earnings

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talyvor/lens/internal/economy"
)

// Gates is the deployment's earning switches, passed in rather than read, so this package does not
// import internal/config (which imports half the tree) and so a caller can report on a hypothetical
// configuration.
//
// ⚠ THESE EXIST SO A ZERO CAN BE TOLD FROM AN OFF SWITCH. Every one of them defaults to FALSE, so a
// stock deployment mints no royalty at all and an earnings figure of 0 is correct AND uninformative.
// A surface that renders a bare "you earned $0.00" over a disabled feature is stating an operator
// setting as a measurement.
type Gates struct {
	EconomyEnabled            bool
	PoolRoyaltyMintingEnabled bool
	CachePoolableEnabled      bool
	DistillPoolableEnabled    bool
}

// disabled names the switches that are off, in the order an operator would turn them on.
func (g Gates) disabled() []string {
	var out []string
	for _, c := range []struct {
		on   bool
		name string
	}{
		{g.EconomyEnabled, "LENS_ECONOMY_ENABLED"},
		{g.PoolRoyaltyMintingEnabled, "LENS_POOL_ROYALTY_MINTING_ENABLED"},
		{g.CachePoolableEnabled, "LENS_CACHE_POOLABLE_ENABLED"},
		{g.DistillPoolableEnabled, "LENS_DISTILL_POOLABLE_ENABLED"},
	} {
		if !c.on {
			out = append(out, c.name)
		}
	}
	return out
}

// TypeLine is one ledger type's contribution to the summary, with the reason it was counted the way
// it was. The reason ships in the response deliberately: a customer-facing earnings number that
// cannot explain its own composition is the shape this repo keeps finding.
type TypeLine struct {
	Type        string `json:"type"`
	Class       Class  `json:"class"`
	Kind        Kind   `json:"kind"`
	AmountULENS int64  `json:"amount_ulens"`
	Rows        int64  `json:"rows"`
	Reason      string `json:"reason"`
}

// Summary is one workspace's earnings.
//
// ⚠ IT DELIBERATELY DOES NOT CARRY lifetime_earned. That column is lifetime CREDITED — every credit
// raises it with no filter on type, so LENS a workspace was given, bought or simply got back all
// count, and a stake/unstake round trip inflates it without bound. Measured and merged as
// docs/lifetime-earned-measured.md (#472). Offering both numbers on one surface would invite the
// question "which is right", and only one of them is.
type Summary struct {
	WorkspaceID string `json:"workspace_id"`

	// ContributionSettledULENS is the number behind "your answers earned this": settled income from
	// something this workspace CONTRIBUTED and somebody else reused.
	ContributionSettledULENS int64 `json:"contribution_settled_ulens"`
	// CapitalSettledULENS is settled income from holding or locking LENS (stake_yield today). Real
	// money, separated because nobody wrote an answer for it.
	CapitalSettledULENS int64 `json:"capital_settled_ulens"`
	// SettledULENS is the two above. Named for what it is rather than "total earned".
	SettledULENS int64 `json:"settled_ulens"`

	// HeldULENS is minted-but-not-settled income, still revocable by adjudication.
	//
	// ⚠ READ FROM lens_token_balances.held_balance, THE COLUMN — NOT from summing `*_held` ledger
	// rows. Finalize decrements the column and writes a positive settled row WITHOUT writing a
	// negative held row, so a rows-sum is everything ever held including everything already paid
	// out. TestR3 pins the difference.
	HeldULENS int64 `json:"held_ulens"`
	// RevokedULENS is the magnitude of income clawed back after adjudication, so a fall in earnings
	// has a name rather than being an unexplained drop.
	RevokedULENS int64 `json:"revoked_ulens"`

	// ── the USD framing, labelled at every turn ───────────────────────────────────────────────
	// W4.6.1 step 7's sentence is in dollars. LENS has one published peg and no market, so these
	// are conversions AT THE PEG and are named that way: nothing here is a price discovered by
	// anyone trading.
	ContributionSettledUSDAtPeg float64 `json:"contribution_settled_usd_at_peg"`
	SettledUSDAtPeg             float64 `json:"settled_usd_at_peg"`
	HeldUSDAtPeg                float64 `json:"held_usd_at_peg"`
	LENSPerUSD                  float64 `json:"lens_per_usd"`

	// EarningEnabled is false when any switch a royalty needs is off. When it is false a zero above
	// says nothing about the workspace.
	EarningEnabled bool     `json:"earning_enabled"`
	DisabledGates  []string `json:"disabled_gates"`

	// ByType is every ledger type present for this workspace, classified, sorted for stable output.
	ByType []TypeLine `json:"by_type"`
	// UnclassifiedTypes are types present in this workspace's ledger that internal/earnings does not
	// know. Reported rather than dropped: a type nobody classified is otherwise silently worth zero.
	UnclassifiedTypes []string `json:"unclassified_types"`
}

// Reader answers the earnings question for one workspace.
type Reader struct{ pool *pgxpool.Pool }

// NewReader returns a Reader. A nil pool yields empty summaries rather than errors, matching
// LedgerStore's posture for DB-less test wiring.
func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{pool: pool} }

const byTypeSQL = `
	SELECT type, COALESCE(SUM(amount), 0)::bigint AS total, COUNT(*)::bigint AS rows
	FROM lens_token_ledger
	WHERE workspace_id = $1
	GROUP BY type`

const heldSQL = `SELECT held_balance::bigint FROM lens_token_balances WHERE workspace_id = $1`

// ForWorkspace summarises what one workspace has earned.
//
// The ledger is the source: it records what MOVED. The six *_mints claim tables record what a mint
// INTENDED, and config.go's ReputationBondedMintingEnabled comment records a measured case where the
// two disagree — the bond reduces the held credit while the claim row keeps the unreduced base.
func (r *Reader) ForWorkspace(ctx context.Context, workspaceID string, gates Gates) (Summary, error) {
	s := Summary{
		WorkspaceID:    workspaceID,
		LENSPerUSD:     economy.LENSPerUSD,
		EarningEnabled: gates.EconomyEnabled && gates.PoolRoyaltyMintingEnabled && (gates.CachePoolableEnabled || gates.DistillPoolableEnabled),
		DisabledGates:  gates.disabled(),
		ByType:         []TypeLine{},
	}
	if r.pool == nil {
		return s, nil
	}

	rows, err := r.pool.Query(ctx, byTypeSQL, workspaceID)
	if err != nil {
		return Summary{}, fmt.Errorf("earnings: by-type query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var total, n int64
		if err := rows.Scan(&typ, &total, &n); err != nil {
			return Summary{}, fmt.Errorf("earnings: scan: %w", err)
		}
		rule := Classify(typ)
		s.ByType = append(s.ByType, TypeLine{
			Type: typ, Class: rule.Class, Kind: rule.Kind,
			AmountULENS: total, Rows: n, Reason: rule.Reason,
		})
		switch rule.Class {
		case Settled:
			if rule.Kind == Capital {
				s.CapitalSettledULENS += total
			} else {
				s.ContributionSettledULENS += total
			}
		case Revoked:
			// Revoke rows are written negative (RevokeHeldTxAs passes -amount), so report the
			// magnitude — "12 µLENS was clawed back" reads correctly, "-12 earned" does not.
			if total < 0 {
				total = -total
			}
			s.RevokedULENS += total
		case Unclassified:
			s.UnclassifiedTypes = append(s.UnclassifiedTypes, typ)
		}
	}
	if err := rows.Err(); err != nil {
		return Summary{}, fmt.Errorf("earnings: rows: %w", err)
	}
	s.SettledULENS = s.ContributionSettledULENS + s.CapitalSettledULENS

	// Held comes from the BALANCE COLUMN. See the field comment: summing `*_held` ledger rows would
	// include every held mint that has since been paid out.
	if err := r.pool.QueryRow(ctx, heldSQL, workspaceID).Scan(&s.HeldULENS); err != nil && err != pgx.ErrNoRows {
		return Summary{}, fmt.Errorf("earnings: held balance: %w", err)
	}

	sort.Slice(s.ByType, func(i, j int) bool { return s.ByType[i].Type < s.ByType[j].Type })
	sort.Strings(s.UnclassifiedTypes)

	s.ContributionSettledUSDAtPeg = usdAtPeg(s.ContributionSettledULENS)
	s.SettledUSDAtPeg = usdAtPeg(s.SettledULENS)
	s.HeldUSDAtPeg = usdAtPeg(s.HeldULENS)
	return s, nil
}

// usdAtPeg converts µLENS to dollars at the published peg: µLENS → LENS (÷1e6) → USD (÷LENSPerUSD).
// The same conversion migration 0103 applied to the royalty margin views when it corrected them for
// subtracting µLENS from dollars.
func usdAtPeg(micro int64) float64 {
	return (float64(micro) / 1_000_000.0) / economy.LENSPerUSD
}
