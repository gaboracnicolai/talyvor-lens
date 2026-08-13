package api

import (
	"context"
	"errors"
	"time"

	"github.com/talyvor/lens/internal/compressmeasure"
)

// COMPRESSION SAVINGS — what the prompt rewriter actually removed from this
// workspace's wire, and how much of that reached its bill.
//
// ⚠ IT IS BYTES AND COUNTS, AND THERE IS NO PERCENTAGE IN IT. This repo has
// rendered a wrong customer-facing number three times from one column family
// (migration 0114), and the rewriter's own SavingsPct cannot see a change len/4
// integer division swallows. A ratio here would be a headline that outruns its
// evidence; the pitch for this layer is ATTRIBUTION, and the attribution is the
// byte totals plus the denominator that says how many requests produced them.
//
// ⚠ AND IT REPORTS ESTIMATED-PATH REQUESTS BESIDE THE BYTES ON PURPOSE. On a
// request the provider returned no usage block for, the spend row is written from
// len(ORIGINAL)/4 — so those bytes were removed from the wire and changed the bill
// by nothing. A summary that showed BytesRemoved alone would describe a saving the
// customer did not get.
type CompressionSavings struct {
	compressmeasure.Summary
	// Days is the window actually used, after clamping.
	Days int `json:"days"`
}

// CompressionSavingsStore reads the aggregate. One method, so the route has one
// dependency. *compressmeasure.Store satisfies it.
type CompressionSavingsStore interface {
	Summarise(ctx context.Context, workspaceID string, since time.Time) (compressmeasure.Summary, error)
}

// ErrNoCompressionSavingsStore is returned when the reader is unconfigured, so
// the route can answer 503 ("not wired") rather than 200 with a zero.
//
// ⚠ THIS DISTINCTION IS THE WHOLE POINT OF THE ENDPOINT, not a nicety. There are
// THREE states a caller must be able to tell apart and only two of them look
// alike: the reader is not wired; the reader is wired and no gated request has
// been measured (which is every workspace today — 0117 backfilled them all to
// 'disabled'); the reader is wired, requests were measured, and they saved
// nothing (which is what 0 of 308 corpus prompts predicts). 503 answers the
// first. Requests=0 answers the second. Requests>0 with BytesRemoved=0 answers
// the third — and it is the answer this feature most expects to give.
var ErrNoCompressionSavingsStore = errors.New("compression savings: no store configured")

// ReadCompressionSavings clamps the window and reads the aggregate.
//
// The clamp is the tenant boundary's friend rather than a convenience: an
// unbounded `days` turns a cheap indexed count into a full scan. 1..90,
// defaulting to 30 — the same bounds as distill usage, on the same reasoning.
func ReadCompressionSavings(ctx context.Context, s CompressionSavingsStore, workspaceID string, days int, now time.Time) (CompressionSavings, error) {
	if s == nil {
		return CompressionSavings{}, ErrNoCompressionSavingsStore
	}
	if days <= 0 {
		days = 30
	}
	if days > 90 {
		days = 90
	}
	sum, err := s.Summarise(ctx, workspaceID, now.AddDate(0, 0, -days))
	if errors.Is(err, compressmeasure.ErrUnwired) {
		// A store with no pool behind it is "not wired", not "measured nothing".
		// Mapped here rather than at the route so a TYPED-NIL store — which is
		// not == nil once it sits in this interface — cannot serve zeroes.
		return CompressionSavings{}, ErrNoCompressionSavingsStore
	}
	if err != nil {
		return CompressionSavings{}, err
	}
	return CompressionSavings{Summary: sum, Days: days}, nil
}
