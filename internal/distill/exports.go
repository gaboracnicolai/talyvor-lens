package distill

// This file exposes pure post-processing primitives for callers that run
// conversion OUTSIDE the in-process Distill/DistillAs entrypoints — notably the
// stage-3 request path, where untrusted bytes are converted in a killable
// subprocess (ProcessIsolator) that returns a faithful Result only. The tier
// and savings steps then run parent-side on that Result.
//
// Both helpers are PURE and never parse untrusted document bytes: ApplyTier
// post-processes already-converted Markdown; ComputeSavings does len/4 math on
// the input length and the converted Markdown. They are thin, intentional
// exports of the existing (already-tested) applyTier/computeSavings.

// ApplyTier applies a fidelity tier to an already-converted Result and records
// it on the Result. faithful is the identity; structured/outline reduce the
// Markdown. Safe to call on a NeedsVision/empty Result (no-op). This is the
// exported, isolated-path equivalent of the WithTier option that the in-process
// Distill/DistillAs path uses.
func ApplyTier(res Result, tier Tier) Result { return applyTier(res, tier) }

// ComputeSavings measures the token reduction of one conversion on the gateway's
// len/4 basis (raw input vs. converted Markdown). cacheHit is false here — this
// is for a freshly-converted Result (e.g. an isolated conversion or a dry-run
// preview), not a cache hit. NeedsVision/empty Results report zero saved (no
// usable Markdown was delivered).
func ComputeSavings(input []byte, res Result) Savings { return computeSavings(input, res, false) }
