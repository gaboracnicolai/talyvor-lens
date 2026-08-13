// Package compressmeasure is the durable record of what the request-path PROMPT
// REWRITER (internal/compressor) actually did to the bytes a provider received,
// and of whether that reached the customer's bill.
//
// It exists because the metric this feature needed already had a tombstone:
// token_events.savings_pct has never had a writer, every row carries the column
// default, and reading it produced a wrong customer-facing number three separate
// times (migration 0114). The rewriter's own SavingsPct is no better as a
// tombstone-free substitute — it is computed on len/4 integer division, so a
// prompt can be MODIFIED while it reads exactly 0.00%. This package therefore
// stores BYTES and a STRING COMPARISON, and derives nothing.
//
// DESCRIPTIVE AND MINT-FREE by construction: the Store holds an Exec/Query
// surface only — no Begin, no ledger handle — so no credit path is reachable from
// a measurement write.
package compressmeasure

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Measurement is one gated request: what the caller sent, what the provider
// received, and what the customer was billed for.
//
// ⚠ Modified IS NOT DERIVED FROM A PERCENTAGE. The writer sets it by comparing
// the two prompt strings at the moment of the rewrite. See compressor.
// TestSavings_ZeroDoesNotMeanUntouched for the case that makes the distinction
// load-bearing rather than pedantic.
type Measurement struct {
	RequestID   string
	WorkspaceID string
	Model       string
	// OriginalBytes is len(the caller's prompt); SentBytes is len(the prompt
	// rebuildBody put on the wire). Bytes, not tokens: tokens here would be the
	// same len/4 approximation that hides one-byte changes.
	OriginalBytes int
	SentBytes     int
	Modified      bool
	// BilledInputTokens is the input-token count the spend row was written with,
	// and CostEstimated says which basis produced it. CostEstimated=true means
	// len(ORIGINAL)/4 — the bytes the rewriter removed moved no money at all.
	//
	// ⚠ BilledInputTokens IS STORED PER ROW AND DELIBERATELY NEVER SUMMED. Summary
	// below carries no total for it, because the two bases are not commensurable:
	// adding an estimated len(ORIGINAL)/4 to a provider-reported count yields a
	// figure whose meaning moves with the mix. The COUNT of each basis is the
	// honest aggregate and is what EstimatedPathRequests reports. See migration
	// 0118 for the same argument where a DBA will find it.
	BilledInputTokens int
	CostEstimated     bool
}

// Summary is the aggregate the reader serves. Every field is a COUNT or a byte
// total; there is deliberately no percentage anywhere in it.
//
// ⚠ Requests IS THE DENOMINATOR AND IT IS NOT DECORATION. "the rewriter ran and
// saved nothing" and "the rewriter never ran" are opposite answers that a
// bytes-only summary renders identically. Requests=0 means no gated request has
// been measured in the window — which is what every workspace looks like today,
// because migration 0117 backfilled every one of them to 'disabled'.
type Summary struct {
	// Requests is every gated request measured in the window, saving or not.
	//
	// ⚠ "MEASURED" IS NARROWER THAN "GATED", and the difference is the one thing a
	// reader of this struct must not assume away. The writer sits inside the serve
	// path's spend-row branch, so the population is gated requests THAT PRODUCED A
	// SPEND ROW (200, not output-blocked, not LoggingNone, alert manager wired),
	// minus observational sheds. A gated request whose upstream answered non-200
	// still had its rewritten prompt sent to a provider and is absent here. See
	// proxy.captureCompression for the enumeration and the tests that pin it.
	//
	// ⚠⚠ AND TWO WHOLE CLASSES OF REQUEST NEVER REACH THE GATE, so they are
	// absent for a reason no narrowing of "gated" can express: a STREAMING
	// request and a CACHE HIT both return from proxy.serve above the gate. A
	// workspace whose policy is `always` therefore compresses none of its
	// streamed traffic and measures none of it, and its cache hits are missing
	// from this count entirely. Measured through the wire in
	// proxy/compression_population_test.go. Read this field as
	// UPSTREAM-SERVED, NON-STREAMED, BILLED gated requests.
	Requests int `json:"requests"`
	// Modified is how many of those had bytes on the wire differing from the
	// bytes the caller sent.
	Modified int `json:"modified"`
	// OriginalBytes/SentBytes are the summed lengths; BytesRemoved is their
	// difference and is a WIRE fact, never a money fact.
	OriginalBytes int `json:"original_bytes"`
	SentBytes     int `json:"sent_bytes"`
	BytesRemoved  int `json:"bytes_removed"`
	// EstimatedPathRequests is how many of Requests were billed on
	// len(ORIGINAL)/4 because the provider reported no usage. On those rows the
	// bytes removed reached the bill by exactly zero, however many they were.
	EstimatedPathRequests int `json:"estimated_path_requests"`
}

// execQuerier is the whole persistence surface: Exec to write, QueryRow to
// summarise. No Begin — a measurement write can never open a transaction that
// reaches a ledger.
type execQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store reads and writes compression_measurements. A nil-pool Store is inert on
// the WRITE side — Record is a no-op, so an unwired deployment cannot fail a
// served request — and LOUD on the read side: Summarise returns ErrUnwired
// rather than a zero summary, because "not connected" and "measured nothing" are
// different answers and only one of them is a measurement.
type Store struct{ db execQuerier }

// NewStore wraps a pool. nil → an inert store.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return &Store{}
	}
	return &Store{db: pool}
}

// ON CONFLICT DO NOTHING: the FIRST measurement for a request stands. A retried
// write cannot inflate the denominator, and it cannot rewrite what was already
// observed about a request that has already been served.
const recordSQL = `INSERT INTO compression_measurements
    (request_id, workspace_id, model, original_bytes, sent_bytes, modified, billed_input_tokens, cost_estimated)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (request_id) DO NOTHING`

// Record persists one gated request's measurement. Inert on a nil store.
func (s *Store) Record(ctx context.Context, m Measurement) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.Exec(ctx, recordSQL,
		m.RequestID, m.WorkspaceID, m.Model,
		m.OriginalBytes, m.SentBytes, m.Modified,
		m.BilledInputTokens, m.CostEstimated)
	return err
}

// COUNT(*) is the denominator and it is counted first, deliberately: every other
// figure below is meaningless without it. COALESCE on the sums because SUM over
// zero rows is NULL, and a NULL scanned into an int is an error, not a zero.
const summarySQL = `
SELECT
  COUNT(*)                                              AS requests,
  COUNT(*) FILTER (WHERE modified)                      AS modified,
  COALESCE(SUM(original_bytes), 0)                      AS original_bytes,
  COALESCE(SUM(sent_bytes), 0)                          AS sent_bytes,
  COUNT(*) FILTER (WHERE cost_estimated)                AS estimated_path_requests
FROM compression_measurements
WHERE workspace_id = $1
  AND created_at >= $2`

// ErrUnwired is returned by Summarise when no pool backs the store.
//
// ⚠ IT IS AN ERROR AND NOT A ZERO SUMMARY, and the reason is a trap this
// codebase has stepped in before: a reader that answers "0 requests, 0 bytes"
// when it is not connected to anything is reporting a structural zero as a
// measurement. It also survives the TYPED-NIL hazard — a (*Store)(nil) stored in
// an interface is not == nil, so a caller's `if store == nil` check would not
// fire and the route would serve confident zeroes. Failing here means the answer
// cannot be produced by an instrument that read nothing.
var ErrUnwired = errors.New("compressmeasure: store has no database")

// Summarise aggregates one workspace's window. The tenant boundary is the WHERE
// clause, not a caller's discipline.
func (s *Store) Summarise(ctx context.Context, workspaceID string, since time.Time) (Summary, error) {
	if s == nil || s.db == nil {
		return Summary{}, ErrUnwired
	}
	var out Summary
	if err := s.db.QueryRow(ctx, summarySQL, workspaceID, since).Scan(
		&out.Requests, &out.Modified, &out.OriginalBytes, &out.SentBytes,
		&out.EstimatedPathRequests,
	); err != nil {
		return Summary{}, err
	}
	out.BytesRemoved = out.OriginalBytes - out.SentBytes
	return out, nil
}
