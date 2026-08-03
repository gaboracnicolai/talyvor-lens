package api

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DISTILL USAGE — a workspace-scoped COUNT of the documents this workspace had converted.
//
// ⚠ WHY NOT /v1/api/distill/summary. That endpoint aggregates the PROCESS-GLOBAL distill counters,
// so serving it to a customer would put other tenants' numbers on their screen. This reads
// token_events, which carries workspace_id, and filters on it — the scoping is in the WHERE clause
// rather than in a caller's discipline.
//
// ⚠ AND IT IS A COUNT, NOT A SAVING. distill_method='convert' rows carry the saving IMPLICITLY in
// their lower input_tokens — there is no separate saving figure to read, and the savings metric
// reads 0 for every format except HTML at the tier the request path uses. A screen promising a
// number the data cannot produce is worse than a screen promising nothing, so this counts
// documents and says so.
type DistillUsage struct {
	// Converted is the number of spend rows whose prompt was distilled to Markdown before the
	// model saw it.
	Converted int `json:"converted"`
	// VisionOCR is the number of OCR sub-call rows — a text-less document (scanned, image-only)
	// read by a vision model. ⚠ A COST, never a saving, and counted separately for that reason.
	VisionOCR int `json:"vision_ocr"`
	Days      int `json:"days"`
}

// DistillUsageStore reads the counts. One method so the route has one dependency.
type DistillUsageStore interface {
	DistillUsage(ctx context.Context, workspaceID string, since time.Time) (converted, visionOCR int, err error)
}

// ErrNoDistillUsageStore is returned when the reader is unconfigured, so the route can answer 503
// ("not wired") rather than 200 with a zero — an absent reader and a workspace that converted
// nothing must not render identically.
var ErrNoDistillUsageStore = errors.New("distill usage: no store configured")

// ReadDistillUsage clamps the window and reads the counts.
//
// ⚠ THE CLAMP IS THE TENANT BOUNDARY'S FRIEND, NOT A CONVENIENCE: an unbounded `days` lets a caller
// turn a cheap indexed count into a full-table scan. 1..90, defaulting to 30.
func ReadDistillUsage(ctx context.Context, s DistillUsageStore, workspaceID string, days int, now time.Time) (DistillUsage, error) {
	if s == nil {
		return DistillUsage{}, ErrNoDistillUsageStore
	}
	if days <= 0 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	conv, vis, err := s.DistillUsage(ctx, workspaceID, now.AddDate(0, 0, -days))
	if err != nil {
		return DistillUsage{}, err
	}
	return DistillUsage{Converted: conv, VisionOCR: vis, Days: days}, nil
}

// pgDistillUsageStore reads the counts straight from token_events.
//
// The predicate matches migration 0040's partial index exactly —
// (workspace_id, distill_method, created_at DESC) WHERE distill_method <> ” — so this stays an
// index scan as the table grows rather than degrading into a seq scan on the busiest table.
type pgDistillUsageStore struct{ q Queryer }

// Queryer is the read surface (a *pgxpool.Pool satisfies it). It mirrors the narrow pgx subset
// other packages here take, so tests can drive it with pgxmock and production passes the pool.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewDistillUsageStore builds the reader over any Queryer.
func NewDistillUsageStore(q Queryer) DistillUsageStore { return pgDistillUsageStore{q: q} }

const distillUsageSQL = `
SELECT
  COUNT(*) FILTER (WHERE distill_method = 'convert')    AS converted,
  COUNT(*) FILTER (WHERE distill_method = 'vision_ocr') AS vision_ocr
FROM token_events
WHERE workspace_id = $1
  AND distill_method <> ''
  AND created_at >= $2`

func (s pgDistillUsageStore) DistillUsage(ctx context.Context, workspaceID string, since time.Time) (int, int, error) {
	if s.q == nil {
		return 0, 0, ErrNoDistillUsageStore
	}
	var conv, vis int
	if err := s.q.QueryRow(ctx, distillUsageSQL, workspaceID, since).Scan(&conv, &vis); err != nil {
		return 0, 0, err
	}
	return conv, vis, nil
}
