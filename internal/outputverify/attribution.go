package outputverify

import (
	"context"
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

// RecordAttributionIfOwned records an attribution ONLY if the caller produced output_id.
//   - owned=false ⇒ the caller is not the producer (handler → 403); no row is written.
//   - recorded=true ⇒ a new attribution row was inserted (handler → 200 recorded:true).
//   - recorded=false ⇒ an attribution for this (output_id, workspace_id, target_kind) already exists
//     (append-only dedup; handler → 200 recorded:false). Distinguishing an IDENTICAL re-post from a
//     CONFLICTING one (a different target_ref → 409) is Property 4 — `conflict` is always false here.
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
	return true, tag.RowsAffected() == 1, false, nil
}
