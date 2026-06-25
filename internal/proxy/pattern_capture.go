package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/workspace"
)

// pattern_capture.go — the Phase-3 routing-pattern CAPTURE WRITE: the producer
// for the already-live routing Advisor (which reads opted_in routing_patterns
// but had no writer). Post-serve, observational, structurally MINT-FREE.
//
// THE SAFETY IS STRUCTURAL: capturePattern returns NOTHING. A void, post-serve,
// error-swallowed call cannot block, delay, fail, or alter any request — there
// is no return value a serve path could branch on. Same shape as the
// shadow-LXC debit and the pooled-royalty mint.
//
// CANNOT MINT: the sink is RecordPatternObservation, which persists an
// anonymized observation and never reaches the ledger (capture and earning are
// separate; earning is a later stage). The sink interface itself exposes only
// the capture method — there is no credit/earn method to call.
//
// DOUBLE-GATED: capture fires iff PatternCaptureEnabled (this flag) AND the
// workspace has opted in. The flag gate is here (the proxy); the opt-in gate is
// in the SQL (RecordPatternObservation's WHERE EXISTS over
// workspace_pattern_optin) — so a non-opted-in workspace gets NO row even when
// the flag is on. Default-off ⇒ no sink call at all.

// patternCaptureSink is the minimal capture surface the proxy depends on — one
// method, persist-only. *mining.PatternMiner.RecordPatternObservation satisfies
// it. Deliberately exposes NO credit/earn method (cannot mint by construction).
type patternCaptureSink interface {
	RecordPatternObservation(ctx context.Context, workspaceID string, p mining.RoutingPattern) error
}

// SetPatternCapture wires the capture sink + its enable flag (read per-call).
// The proxy holds both as optional, nil-safe fields.
func (p *Proxy) SetPatternCapture(sink patternCaptureSink, enabled func() bool) {
	p.patternSink = sink
	p.patternCaptureEnabled = enabled
}

// SetObservationalLimiter installs the shared bound on post-serve
// observational writes (a nil limiter admits everything). Capture runs
// inline post-flush, so without a bound its concurrency equals total
// in-flight requests — under overload that converts offered load directly
// into pool-connection churn (#122; 2026-06-11 load trial).
func (p *Proxy) SetObservationalLimiter(l *backpressure.Limiter) {
	p.obsLimiter = l
}

// captureWriteTimeout bounds the detached observation write. Mirrors
// attribution.RecordAsync's 5s budget. Without a deadline a pool-exhausted
// Acquire blocks the (post-flush) serve goroutine indefinitely — the exact
// #122 complaint.
const captureWriteTimeout = 5 * time.Second

// capturePattern records a routing observation for a just-served request. VOID
// by design — it cannot affect the response. Called POST-SERVE on a detached
// context (so client cancellation can't drop the write, and the write can't
// touch the serve). Errors are logged-and-swallowed.
//
// CAPTURE ONLY SCORED RESPONSES: scored is false whenever the response carries
// no real quality score (a served non-200 passthrough, or no scorer wired). An
// unscored observation would be written with output_quality=0 and drag down the
// Advisor's quality average for that model — the same poison the streaming
// deferral avoids. So unscored ⇒ no capture. This is the unifying invariant:
// streaming (no scorer on that path) and non-200 (scorer skipped) are both
// unscored, hence both uncaptured.
func (p *Proxy) capturePattern(ctx context.Context, piiDetected, guardrailFired bool, loggingPolicy workspace.LoggingPolicy, workspaceID, feature, model, provider string, inputTokens, outputTokens int, quality float64, scored bool, latencyMs int64, cacheHit bool, complexityBucket string) {
	if p == nil || p.patternSink == nil || p.patternCaptureEnabled == nil || !p.patternCaptureEnabled() {
		return
	}
	// CORPUS EXCLUSION (shared guard): a sensitive request is never captured —
	// the same predicate as the earn path (sensitiveForCorpus), so the two
	// writers can never drift. See pattern_corpus_sensitive.go.
	if sensitiveForCorpus(piiDetected, guardrailFired, loggingPolicy) {
		return
	}
	if !scored {
		return // no real quality score ⇒ don't poison the Advisor with quality=0
	}
	// Shed under overload: capture is observational by contract, so when the
	// writer bound is saturated the correct backpressure is to skip the row,
	// not to queue another writer against an already-drowning pool.
	if !p.obsLimiter.TryAcquire() {
		if p.obsLimiter.LogDrop() {
			slog.Warn("pattern capture: observation dropped (writer bound reached; observational, serve unaffected)",
				slog.Int64("dropped_total", p.obsLimiter.Dropped()))
		}
		return
	}
	defer p.obsLimiter.Release()
	pat := mining.ExtractPattern(feature, model, provider, inputTokens, outputTokens, quality, latencyMs, cacheHit)
	pat.ComplexityBucket = complexityBucket // tier-cohort dimension (worktier.ComplexityBucketFor); '' when the projection is unavailable
	// Detached: post-serve, must outlive client cancellation and cannot affect
	// the served response. The opt-in WRITE gate lives in the sink's SQL.
	// Deadline-bounded (#122): detachment must not mean "can hang forever".
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if err := p.patternSink.RecordPatternObservation(wctx, workspaceID, pat); err != nil {
		slog.Warn("pattern capture: observation write failed (observational; serve unaffected)",
			slog.String("workspace", workspaceID),
			slog.String("err", err.Error()),
		)
	}
}
