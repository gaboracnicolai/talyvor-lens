package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/talyvor/lens/internal/outputverify"
)

// outputVerdictSink is the WRITE-ONLY persistence surface the K4 verifier needs — just Record.
// *outputverify.Writer satisfies it. The sink CANNOT mint: outputverify imports no ledger and is
// import-guarded (see internal/outputverify).
type outputVerdictSink interface {
	Record(ctx context.Context, r outputverify.VerdictRecord) (bool, error)
}

// SetOutputVerifier wires the K4 intrinsic verifier sink + its enable flag (read per-call). main wires it
// default-off (LENS_K4_VERIFIER_ENABLED). nil/off → no verification runs and no verdicts are written.
func (p *Proxy) SetOutputVerifier(sink outputVerdictSink, enabled func() bool) {
	p.outputVerdictSink = sink
	p.outputVerdictEnabled = enabled
}

// deriveOutputID returns the gateway-bound output id + the prompt/response hashes. The SAME inputs used at
// the header seam (X-Talyvor-Output-Id, pre-flush) and at the post-flush capture produce the SAME id — so
// the header the caller receives EQUALS the stored verdict row's output_id, and (via the identity's
// workspace binding) the ownership check on the mechanical report-back holds.
func deriveOutputID(workspaceID, model, prompt string, response []byte, servedAt time.Time) (id, promptHash, responseHash string) {
	promptHash = outputverify.Sha256Hex([]byte(prompt))
	responseHash = outputverify.Sha256Hex(response)
	id = outputverify.DeriveOutputID(workspaceID, model, promptHash, responseHash, servedAt.Unix())
	return id, promptHash, responseHash
}

// outputVerdictOn reports whether the K4 verifier is wired + enabled (used to gate the X-Talyvor-Output-Id
// header at the pre-flush seam).
func (p *Proxy) outputVerdictOn() bool {
	return p != nil && p.outputVerdictSink != nil && p.outputVerdictEnabled != nil && p.outputVerdictEnabled()
}

// captureOutputVerdict derives the gateway-bound output identity, runs the INTRINSIC verifier over the
// constraint the REQUEST declared + the served response, and records exactly ONE verdict — POST-FLUSH (off
// the hot path), best-effort, void. It shares captureWorkTier's obsLimiter budget + detached-write
// discipline: it runs AFTER the response is already flushed to the caller, so it can NEVER add latency,
// block, or alter the response. It touches NO money (the sink has no ledger handle; outputverify is
// import-guarded). Hashes only — the raw prompt/response text is never persisted.
//
// served-at bucket = 1-second granularity: each serving EVENT gets a distinct identity (the same output
// re-served later is a distinct bondable event), while an exact same-second replay dedups via ON CONFLICT.
func (p *Proxy) captureOutputVerdict(ctx context.Context, workspaceID, model, provider string,
	requestBody, responseBody []byte, prompt string, servedAt time.Time) {
	if p == nil || p.outputVerdictSink == nil || p.outputVerdictEnabled == nil || !p.outputVerdictEnabled() {
		return
	}
	// Shed under overload, sharing pattern capture's writer bound (mirrors captureWorkTier).
	if p.obsLimiter != nil {
		if !p.obsLimiter.TryAcquire() {
			if p.obsLimiter.LogDrop() {
				slog.Warn("outputverify: verdict dropped (writer bound reached; observational, serve unaffected)",
					slog.Int64("dropped_total", p.obsLimiter.Dropped()))
			}
			return
		}
		defer p.obsLimiter.Release()
	}

	outputID, promptHash, responseHash := deriveOutputID(workspaceID, model, prompt, responseBody, servedAt)
	res := outputverify.Verify(requestBody, responseBody)

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), captureWriteTimeout)
	defer cancel()
	if _, err := p.outputVerdictSink.Record(wctx, outputverify.VerdictRecord{
		OutputID: outputID, WorkspaceID: workspaceID, Model: model,
		Verdict: res.Verdict, Reason: res.Reason, ConstraintKind: res.ConstraintKind,
		PromptSHA256: promptHash, ResponseSHA256: responseHash,
	}); err != nil {
		slog.Warn("outputverify: verdict write failed (observational; serve unaffected)",
			slog.String("workspace", workspaceID), slog.String("err", err.Error()))
	}
}
