package outputverify

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Attribution target kinds — a closed enum (validated at the handler). target_ref is opaque.
const (
	AttrKindPR   = "pr"
	AttrKindSpec = "spec"
)

// attributionDB needs a read (ownership EXISTS against k4_output_verdicts) + the append-only write.
// It exposes no Begin — no arbitrary transaction surface. *pgxpool.Pool satisfies it. Deliberately
// NO ledger/mint/held/supply seam: attribution ≠ settlement, and this path touches no amount.
type attributionDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Attribution is one attribution of a produced output to a PR or spec.
type Attribution struct {
	OutputID    string
	WorkspaceID string // the CALLER — must own output_id
	TargetKind  string // "pr" | "spec"
	TargetRef   string // OPAQUE free string — never parsed, never dereferenced
}

// AttributionWriter records output→(PR|spec) attributions, ownership-bound + append-only.
type AttributionWriter struct{ db attributionDB }

func NewAttributionWriter(db attributionDB) *AttributionWriter { return &AttributionWriter{db: db} }

const attrOwnsOutputSQL = `SELECT EXISTS (SELECT 1 FROM k4_output_verdicts WHERE output_id = $1 AND workspace_id = $2)`

// insertAttributionIfOwnedSQL inserts ONLY WHERE the caller owns output_id (it appears in
// k4_output_verdicts with the caller's workspace_id), append-only on (output_id, workspace_id,
// target_kind). The WHERE EXISTS is the ATOMIC ownership guard (mirrors mechanical.go); ON CONFLICT
// DO NOTHING makes a re-post write nothing (recorded=false) and NEVER overwrites the first row.
const insertAttributionIfOwnedSQL = `INSERT INTO output_attributions
    (output_id, workspace_id, target_kind, target_ref)
SELECT $1, $2, $3, $4
WHERE EXISTS (SELECT 1 FROM k4_output_verdicts WHERE output_id = $1 AND workspace_id = $2)
ON CONFLICT (output_id, workspace_id, target_kind) DO NOTHING`

// existingAttributionRefSQL reads the target_ref already recorded for (output_id, workspace_id,
// target_kind), to distinguish an IDENTICAL re-post (same ref → idempotent, recorded:false) from a
// CONFLICTING one (a different ref → append-only refuse, 409). The row is append-only (ON CONFLICT DO
// NOTHING never overwrites), so this read is stable and reflects the FIRST-wins target_ref.
const existingAttributionRefSQL = `SELECT target_ref FROM output_attributions WHERE output_id = $1 AND workspace_id = $2 AND target_kind = $3`

// RecordAttributionIfOwned records an attribution ONLY if the caller produced output_id.
//   - owned=false ⇒ the caller is not the producer (handler → 403); no row is written.
//   - recorded=true ⇒ a new attribution row was inserted (handler → 200 recorded:true).
//   - recorded=false, conflict=false ⇒ an IDENTICAL attribution already exists (idempotent no-op;
//     handler → 200 recorded:false).
//   - conflict=true ⇒ a DIFFERENT target_ref is already recorded for this (output_id, workspace_id,
//     target_kind) — append-only first-wins; the re-attribution is refused (handler → 409) and the
//     original row is UNCHANGED.
//
// No amount is read, written, summed, or rounded on any path — attribution ≠ settlement.
func (w *AttributionWriter) RecordAttributionIfOwned(ctx context.Context, a Attribution) (owned, recorded, conflict bool, err error) {
	if w == nil || w.db == nil {
		return false, false, false, nil
	}
	if err := w.db.QueryRow(ctx, attrOwnsOutputSQL, a.OutputID, a.WorkspaceID).Scan(&owned); err != nil {
		return false, false, false, fmt.Errorf("outputverify: attribution ownership check: %w", err)
	}
	if !owned {
		return false, false, false, nil
	}
	tag, err := w.db.Exec(ctx, insertAttributionIfOwnedSQL, a.OutputID, a.WorkspaceID, a.TargetKind, a.TargetRef)
	if err != nil {
		return true, false, false, fmt.Errorf("outputverify: record attribution: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, true, false, nil // newly attributed
	}
	// Owned but nothing inserted ⇒ the ON CONFLICT path: a row already exists for this
	// (output_id, workspace_id, target_kind). Compare the stored (first-wins) target_ref to decide
	// idempotent vs conflict.
	var existing string
	if err := w.db.QueryRow(ctx, existingAttributionRefSQL, a.OutputID, a.WorkspaceID, a.TargetKind).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unreachable while k4_output_verdicts rows are append-only (owned held, so the insert's WHERE
			// EXISTS held); treat as a benign idempotent no-op rather than a spurious conflict.
			return true, false, false, nil
		}
		return true, false, false, fmt.Errorf("outputverify: attribution existing-ref read: %w", err)
	}
	return true, false, existing != a.TargetRef, nil
}
