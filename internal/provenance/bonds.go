// Package provenance is H5.β PROVENANCE BONDS — THE MONEY PATH. A workspace stakes collateral on a specific
// gateway-bound output_id (a code generation it relies on). If the mechanical verdict for that output is a
// self-reported FAILURE (the ONLY slash-usable signal — outputverify.IsSlashUsable), the bond is SLASHED:
// the collateral is BURNED via mining.SlashStake (supply reduced, paid to NOBODY). If no slash-usable
// verdict exists by the appeal deadline, the bond RELEASES (collateral returned). TIME releases a bond,
// never a self-attestation — a self-reported PASS proves nothing and NEVER releases it early.
//
// This is money. It moves value ONLY through the existing integer-µLENS mining ledger
// (LockStake/SlashStake/ReleaseStake — no new mint type, no new ledger). It takes the PRIMARY pool (see the
// U8/U9 readrouting invariant). Every amount is BIGINT µLENS — never float.
package provenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/outputverify"
)

var (
	ErrBadAmount = errors.New("provenance: bond amount must be positive µLENS")
	ErrNotOwned  = errors.New("provenance: workspace does not own this output_id")
)

// ledger is the EXACT value-movement seam H5 needs from mining.LedgerStore — all integer µLENS, all within
// a caller-supplied tx so the bond-status CAS and the value movement commit or roll back together.
// *mining.LedgerStore satisfies it. There is no Credit/mint here: a slash only ever BURNS.
type ledger interface {
	LockStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
	SlashStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
	ReleaseStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
}

// beginner is the tx seam (*pgxpool.Pool satisfies it) — kept an interface for testing.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// BondManager is the money core.
type BondManager struct {
	db           beginner
	ledger       ledger
	appealWindow time.Duration
	slashBps     int
	now          func() time.Time
}

// NewBondManager wires the PRIMARY pool + the integer-µLENS ledger. appealWindow is the contestable
// holdback before a burn finalizes; slashBps is the fraction of the bond slashed on a failure (bps of
// 10000; 10000 = the whole bond).
func NewBondManager(pool *pgxpool.Pool, led ledger, appealWindow time.Duration, slashBps int) *BondManager {
	if appealWindow <= 0 {
		appealWindow = 72 * time.Hour
	}
	if slashBps <= 0 || slashBps > 10000 {
		slashBps = 10000
	}
	return &BondManager{db: pool, ledger: led, appealWindow: appealWindow, slashBps: slashBps, now: time.Now}
}

// BondID is server-derived — one bond per (workspace, output). A workspace can only produce a valid bond_id
// over an output_id, and (via CreateBond's ownership check) only over an output it produced.
func BondID(workspaceID, outputID string) string {
	sum := sha256.Sum256([]byte("h5_bond:" + workspaceID + ":" + outputID))
	return hex.EncodeToString(sum[:])
}

// slashKey is the SERVER-DERIVED idempotency key (SEC-11: it contains EVERY identity it protects — the bond,
// the output, and the exact verdict authorizing the burn). Never a caller-supplied id.
func slashKey(bondID, outputID, verdict, source string) string {
	sum := sha256.Sum256([]byte("h5_slash:" + bondID + ":" + outputID + ":" + verdict + ":" + source))
	return hex.EncodeToString(sum[:])
}

// slashAmount = floor(amount * bps / 10000), OVERFLOW-SAFE via big.Int. House rounding is FLOOR: the burn is
// always ≤ the exact fraction and ≤ the bond (bps ≤ 10000), and a sub-µLENS remainder is NEVER burned and
// NEVER minted — rounding can only ever burn LESS, never create value.
func slashAmount(amount int64, bps int) int64 {
	r := new(big.Int).Mul(big.NewInt(amount), big.NewInt(int64(bps)))
	r.Quo(r, big.NewInt(10000)) // truncated (floor for non-negative) integer division
	return r.Int64()            // safe: result ≤ amount, which fits int64
}

const insertBondSQL = `INSERT INTO provenance_bonds
    (bond_id, workspace_id, output_id, amount_ulens, slash_bps, appeal_deadline)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (bond_id) DO NOTHING`

// CreateBond locks amountUlens µLENS as collateral and records an ACTIVE bond on output_id — but ONLY if the
// workspace OWNS output_id (it appears in k4_output_verdicts with this workspace_id), so a bond can only
// ever be on the bonder's OWN output. Idempotent per (workspace, output): a re-create is a no-op (no
// double-lock). The bond insert + the collateral lock are ONE transaction.
func (m *BondManager) CreateBond(ctx context.Context, workspaceID, outputID string, amountUlens int64) (bondID string, created bool, err error) {
	if amountUlens <= 0 {
		return "", false, ErrBadAmount
	}
	bondID = BondID(workspaceID, outputID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return bondID, false, fmt.Errorf("provenance: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owns bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM k4_output_verdicts WHERE output_id=$1 AND workspace_id=$2)`, outputID, workspaceID).Scan(&owns); err != nil {
		return bondID, false, fmt.Errorf("provenance: ownership: %w", err)
	}
	if !owns {
		return bondID, false, ErrNotOwned
	}
	tag, err := tx.Exec(ctx, insertBondSQL, bondID, workspaceID, outputID, amountUlens, m.slashBps, m.now().Add(m.appealWindow))
	if err != nil {
		return bondID, false, fmt.Errorf("provenance: insert bond: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Already bonded — do NOT lock again (idempotent).
		if err := tx.Commit(ctx); err != nil {
			return bondID, false, err
		}
		return bondID, false, nil
	}
	if err := m.ledger.LockStakeTx(ctx, tx, workspaceID, amountUlens, map[string]interface{}{"bond_id": bondID, "output_id": outputID}); err != nil {
		return bondID, false, fmt.Errorf("provenance: lock collateral: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return bondID, false, err
	}
	return bondID, true, nil
}

// SettleBond finalizes a bond. BEFORE the appeal deadline it burns NOTHING (it may open the appeal window
// active→appealing if a slash is pending). AT/AFTER the deadline: a slash-usable verdict → SLASH (burn a
// fraction, supply reduced, to NOBODY); otherwise → RELEASE (full collateral returned). Claim-then-act: the
// status CAS + the ledger op are ONE transaction; a concurrent settle finds RowsAffected==0 and skips — no
// double-slash, no replay. Returns the outcome string.
func (m *BondManager) SettleBond(ctx context.Context, bondID string) (outcome string, err error) {
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("provenance: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ws, outputID, status string
	var amount int64
	var bps int
	var deadline time.Time
	err = tx.QueryRow(ctx,
		`SELECT workspace_id, output_id, amount_ulens, slash_bps, status, appeal_deadline
		 FROM provenance_bonds WHERE bond_id=$1 FOR UPDATE`, bondID).
		Scan(&ws, &outputID, &amount, &bps, &status, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return "not_found", tx.Commit(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("provenance: read bond: %w", err)
	}
	if status != "active" && status != "appealing" {
		return "settled_already", tx.Commit(ctx) // already slashed or released
	}

	verdict, source, hasVerdict := slashUsableVerdict(ctx, tx, outputID, ws)

	if m.now().Before(deadline) {
		// APPEAL WINDOW OPEN — never burn. Mark appealing if a slash is pending (contestable), else nothing.
		if hasVerdict && status == "active" {
			if _, err := tx.Exec(ctx, `UPDATE provenance_bonds SET status='appealing' WHERE bond_id=$1 AND status='active'`, bondID); err != nil {
				return "", fmt.Errorf("provenance: mark appealing: %w", err)
			}
			return "appealing", tx.Commit(ctx)
		}
		return "pending", tx.Commit(ctx)
	}

	// DEADLINE REACHED.
	if hasVerdict {
		// SLASH — CAS the status (claim) then BURN in the same tx.
		key := slashKey(bondID, outputID, verdict, source)
		tag, err := tx.Exec(ctx,
			`UPDATE provenance_bonds SET status='slashed', slash_key=$2, settled_at=now()
			 WHERE bond_id=$1 AND status IN ('active','appealing') AND slash_key IS NULL`, bondID, key)
		if err != nil {
			return "", fmt.Errorf("provenance: claim slash: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "settled_already", tx.Commit(ctx) // a concurrent settle already won — no double-slash
		}
		burn := slashAmount(amount, bps)
		if burn > 0 {
			if err := m.ledger.SlashStakeTx(ctx, tx, ws, burn, map[string]interface{}{"bond_id": bondID, "output_id": outputID, "verdict": verdict}); err != nil {
				return "", fmt.Errorf("provenance: burn: %w", err)
			}
		}
		return "slashed", tx.Commit(ctx)
	}

	// NO slash-usable verdict by the deadline → RELEASE the full collateral.
	tag, err := tx.Exec(ctx,
		`UPDATE provenance_bonds SET status='released', settled_at=now()
		 WHERE bond_id=$1 AND status IN ('active','appealing')`, bondID)
	if err != nil {
		return "", fmt.Errorf("provenance: claim release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return "settled_already", tx.Commit(ctx)
	}
	if err := m.ledger.ReleaseStakeTx(ctx, tx, ws, amount, map[string]interface{}{"bond_id": bondID}); err != nil {
		return "", fmt.Errorf("provenance: release: %w", err)
	}
	return "released", tx.Commit(ctx)
}

// slashUsableVerdict returns a slash-usable verdict for outputID that was reported by the bonder (bondWS).
// The bonder-match is defense-in-depth: combined with 0085 ownership (only the owner can report a verdict on
// an output) and CreateBond's ownership check (a bond is only on the bonder's own output), the ONLY thing
// that can authorize a burn is the bonder's OWN self-reported failure OR Talyvor's OWN attested failure.
// Workspace B can neither report a verdict on A's output nor slash A's bond.
//
// ATTESTED-COMPILED BLOCK: if Talyvor's OWN sandbox attested that the output COMPILES (talyvor_verified +
// compiled), NO slash may occur — Talyvor's reproduced fact beats a self-reported failure, in the workspace's
// FAVOUR (the safe direction: never burn when our own build says the code is fine). Only the attestor writes
// talyvor_verified, so this cannot be forged.
func slashUsableVerdict(ctx context.Context, tx pgx.Tx, outputID, bondWS string) (verdict, source string, ok bool) {
	rows, err := tx.Query(ctx, `SELECT verdict, verdict_source, workspace_id FROM k4_mechanical_verdicts WHERE output_id=$1`, outputID)
	if err != nil {
		return "", "", false
	}
	defer rows.Close()
	var sV, sS string
	var haveSlash, attestedCompiled bool
	for rows.Next() {
		var v, s, vws string
		if err := rows.Scan(&v, &s, &vws); err != nil {
			return "", "", false
		}
		if vws != bondWS {
			continue
		}
		if s == outputverify.SourceTalyvorVerified && v == outputverify.MechCompiled {
			attestedCompiled = true
		}
		if !haveSlash && outputverify.IsSlashUsable(v, s) {
			sV, sS, haveSlash = v, s, true
		}
	}
	if attestedCompiled {
		return "", "", false // Talyvor reproduced a successful compile → block the slash.
	}
	return sV, sS, haveSlash
}
