package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/compressmeasure"
)

// compressionSink is the WRITE-ONLY persistence surface the compression
// measurement needs — just Record. *compressmeasure.Store satisfies it. The sink
// cannot mint: compressmeasure holds an Exec/QueryRow surface and no ledger
// handle.
type compressionSink interface {
	Record(ctx context.Context, m compressmeasure.Measurement) error
}

// SetCompressionSink wires the durable record of what the prompt rewriter did.
//
// ⚠ THERE IS DELIBERATELY NO SECOND ENABLE FLAG, and that is an argument rather
// than an omission. Every other descriptive capture in this package carries its
// own LENS_*_ENABLED because it fires on every served request. This one fires
// ONLY when the 0117 compression gate opened, and that gate is 'disabled' for
// every workspace that exists (0117 backfilled them all). A separate flag would
// therefore let an operator turn the REWRITER on while leaving the MEASUREMENT
// off — which is precisely the order of operations that produced a live rewriter
// nobody could measure in the first place. Wiring the measurement to the gate
// means the feature cannot run unobserved.
func (p *Proxy) SetCompressionSink(sink compressionSink) {
	p.compressionSink = sink
}

// compressionObservation is what the serve path knows by the time the response is
// flushed: the two prompts, and what the spend row was actually written with.
type compressionObservation struct {
	requestID   string
	workspaceID string
	model       string
	// original is the caller's prompt; sent is what rebuildBody put on the wire.
	// Both are carried as STRINGS so the comparison below is a string comparison.
	original string
	sent     string
	// billedInputTokens / costEstimated are the spend row's own figures, copied
	// after the usage block resolved them.
	billedInputTokens int
	costEstimated     bool
}

// captureCompression records one gated request — POST-FLUSH, detached, void,
// best-effort. A persist failure is logged and swallowed; the served response is
// already on the wire and is never affected.
//
// ⚠ IT RECORDS THE ZEROES TOO. A request where the rewriter changed nothing still
// writes a row, because that row is the denominator: without it "ran 10,000 times
// and saved nothing" is indistinguishable from "never ran", and the first of those
// is the measured truth about this rewriter (0 of 308 committed corpus prompts
// modified).
//
// ⚠ BUT THE DENOMINATOR IS NOT "EVERY GATED REQUEST", AND SAYING SO WOULD BE THE
// EASIEST WRONG NUMBER IN THIS FILE. The call site sits inside two enclosing
// conditions it does not own (proxy.go): the 200-and-not-output-blocked branch,
// and the `alertManager != nil && loggingPolicy != LoggingNone` branch that also
// gates the spend row. Add this function's own obsLimiter shed and the population
// is exactly:
//
//	gated requests THAT PRODUCED A SPEND ROW, minus observational sheds.
//
// That is a good population — every row has a real billed_input_tokens beside it,
// which is the only reason the money half of this measurement means anything — but
// it is NOT the set of requests the rewriter touched. A request whose upstream
// answered non-200, whose output a guardrail blocked, or whose workspace runs
// LoggingNone, had its prompt rewritten and sent and is absent from the count.
// Anyone dividing bytes_removed by requests is dividing by the billed subset.
// TestMeasure_LoggingNoneRecordsNothing and TestMeasure_AnUpstreamFailureRecordsNothing
// pin two of those exclusions so this paragraph cannot quietly become true or false.
//
// ⚠ modified IS A STRING COMPARISON. Deriving it from the rewriter's SavingsPct
// would miss every change len/4 integer division swallows — a blank line removed
// inside a fenced code block is a real change to the bytes a provider receives and
// a 0.00% saving at the same time (compressor.TestSavings_ZeroDoesNotMeanUntouched).
// TestMeasure_ModifiedIsAStringComparisonNotAPercentage is what fails if this is
// ever rewritten as a threshold on a number.
func (p *Proxy) captureCompression(ctx context.Context, obs compressionObservation) {
	if p == nil || p.compressionSink == nil {
		return
	}
	if obs.requestID == "" {
		return
	}
	// Shed under overload, sharing the pattern/worktier writer bound.
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("compressmeasure: observation dropped (writer bound reached; observational, serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if err := p.compressionSink.Record(wctx, compressmeasure.Measurement{
		RequestID:         obs.requestID,
		WorkspaceID:       obs.workspaceID,
		Model:             obs.model,
		OriginalBytes:     len(obs.original),
		SentBytes:         len(obs.sent),
		Modified:          obs.sent != obs.original,
		BilledInputTokens: obs.billedInputTokens,
		CostEstimated:     obs.costEstimated,
	}); err != nil {
		slog.Warn("compressmeasure: observation write failed (observational; serve unaffected)",
			slog.String("workspace", obs.workspaceID), slog.String("err", err.Error()))
	}
}
