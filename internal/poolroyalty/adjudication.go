// adjudication.go — the pool-mint ADJUDICATION writer (Stage 3): the gate that
// BINDS a deliberate operator decision to the held-burn, so a production revoke
// can NEVER happen without a preceding, durable audit record.
//
// This is the Revoker's FIRST and ONLY production caller. The never-auto-act
// invariant is structural: Adjudicate takes an EXPLICIT operator-chosen subset
// of request_ids (RevokeRequestIDs) and revokes EXACTLY those — it never
// re-runs the detector, never auto-selects, and the only code path to
// RevokeHeldMints in production runs through here. The endpoint that calls this
// is admin-gated; the whole path is behind LENS_POOL_ROYALTY_MINTING_ENABLED
// (no held rows exist otherwise) — doubly inert in the current config.
//
// RECORD-BEFORE-REVOKE ORDERING (the audit-integrity property):
//
//  1. INSERT the adjudication row (decision + candidate set + chosen subset +
//     decided_by), outcome NULL  → the decision is on disk BEFORE any burn.
//  2. RevokeHeldMints(chosen subset)  → the Revoker's existing per-row txns.
//  3. UPDATE the row SET outcome = <RevokeReport>  → complete the record.
//
// These are deliberately NOT one transaction — the Revoker uses per-row txns by
// design, and one decision spans N rows. The binding is the ORDERING: the
// record exists first, so a crash mid-revoke still leaves "operator decided to
// revoke set Z" durable (outcome NULL). The claim rows (status='revoked') are
// the authoritative money truth; the record's outcome is reconciled against
// them. If the INSERT itself fails, the revoke does NOT fire (no record ⇒ no
// burn).
//
// The writer COMPOSES the existing Revoker (it calls RevokeHeldMints, never
// changes it) and a small record-write surface; it widens no existing interface.
package poolroyalty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AdjudicationDecision is the operator's reviewed decision — the required input
// to a production revoke. RevokeRequestIDs is the operator-chosen SUBSET of the
// reviewed candidates; only these are revoked (the human's narrowing of the
// resolver's over-selected set is honored exactly).
type AdjudicationDecision struct {
	FlagType            string   // volume | self_dealing | similarity
	ResolutionLabel     string   // the resolver's honest over-selection label
	CandidateRequestIDs []string // the set the operator reviewed (the resolver output)
	RevokeRequestIDs    []string // the subset the operator chose to revoke
	DecidedBy           string   // AuthContext.UserID or 'global_key'
}

// adjudicationDB is the minimal record-write surface (QueryRow for the
// RETURNING INSERT, Exec for the completing UPDATE). *pgxpool.Pool satisfies it.
type adjudicationDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// revokerSurface is the held-burn orchestration the writer CALLS — satisfied by
// *Revoker.RevokeHeldMints. The writer never widens or changes it.
type revokerSurface interface {
	RevokeHeldMints(ctx context.Context, requestIDs []string) (RevokeReport, error)
}

// AdjudicationWriter binds record→revoke. The nil writer is inert.
type AdjudicationWriter struct {
	db     adjudicationDB
	revoke revokerSurface
}

// NewAdjudicationWriter composes the record-write surface and the Revoker.
func NewAdjudicationWriter(db adjudicationDB, revoke revokerSurface) *AdjudicationWriter {
	return &AdjudicationWriter{db: db, revoke: revoke}
}

const insertAdjudicationSQL = `INSERT INTO pool_royalty_adjudications
    (flag_type, resolution_label, candidate_request_ids, revoked_request_ids, decided_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`

const completeAdjudicationSQL = `UPDATE pool_royalty_adjudications
SET outcome = $2
WHERE id = $1`

// Adjudicate writes the decision record FIRST, then revokes exactly the chosen
// subset, then completes the record with the RevokeReport. Returns the
// adjudication id and the report. A per-row revoke error is recorded in the
// report (the Revoker never fails the call); only a record-write failure or a
// nil/empty input errors Adjudicate.
func (w *AdjudicationWriter) Adjudicate(ctx context.Context, d AdjudicationDecision) (string, RevokeReport, error) {
	if w == nil || w.db == nil || w.revoke == nil {
		return "", RevokeReport{}, errors.New("poolroyalty: nil adjudication writer")
	}
	if len(d.RevokeRequestIDs) == 0 {
		return "", RevokeReport{}, errors.New("poolroyalty: empty revoke set — nothing to adjudicate")
	}

	// 1. Record the decision BEFORE any burn.
	var id string
	if err := w.db.QueryRow(ctx, insertAdjudicationSQL,
		d.FlagType, d.ResolutionLabel, d.CandidateRequestIDs, d.RevokeRequestIDs, d.DecidedBy,
	).Scan(&id); err != nil {
		return "", RevokeReport{}, fmt.Errorf("poolroyalty: write adjudication record: %w", err)
	}

	// 2. Revoke exactly the operator-chosen subset (the Revoker's per-row txns).
	report, _ := w.revoke.RevokeHeldMints(ctx, d.RevokeRequestIDs)

	// 3. Complete the record with the outcome. A failure here leaves the record
	// with outcome NULL — the decision and the claim rows remain authoritative;
	// we surface the error but the burn already (durably) happened and is
	// recorded on the claim rows.
	outcomeJSON, merr := json.Marshal(report)
	if merr != nil {
		return id, report, fmt.Errorf("poolroyalty: marshal outcome: %w", merr)
	}
	if _, err := w.db.Exec(ctx, completeAdjudicationSQL, id, outcomeJSON); err != nil {
		return id, report, fmt.Errorf("poolroyalty: complete adjudication record: %w", err)
	}
	return id, report, nil
}
