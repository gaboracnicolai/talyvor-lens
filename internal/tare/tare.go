// Package tare is the Talyvor context-reduction layer — Phase 1a: the Reduction
// interface and the deterministic JSON compressor.
//
// ⚠ THE PLACEMENT, AND WHY IT IS NOT WHERE THE DESIGN PUTS IT. reports/tare-design-v1.html places
// Tare as an ext_proc filter in the Envoy gateway. edge-infra has never run in any cluster —
// production is docker compose behind Caddy, with no Envoy in the path — so a Tare built as
// specified would run NOWHERE. It is built here instead, behind an interface that can move to
// ext_proc unchanged: the metering, holdout, attribution and prefix-stability are agnostic to
// placement, and the filter position is plumbing. W6.1.1 states this resolution; this package
// implements it rather than re-deciding it.
//
// ⚠ NOTHING CALLS THIS. Phase 1a adds no call site on the serve path; tests drive it directly.
package tare

import "context"

// Kind names the content class a reducer is being asked to handle. The router that dispatches on
// it is Phase 1c; Phase 1a ships one reducer and the kind it accepts.
type Kind string

const (
	KindJSON    Kind = "json"
	KindCode    Kind = "code"
	KindLog     Kind = "log"
	KindProse   Kind = "prose"
	KindUnknown Kind = "unknown"
)

// Reduction is the seam. The signature is W6.1.1's, verbatim.
//
// ⚠ IT MUST BE ABLE TO REFUSE, AND REFUSAL IS `reduced == content, err == nil`. Returning the
// input unchanged is a valid outcome, not a failure — "the compressor that shipped before had no
// way to decline". An error is reserved for a reducer that could not run at all; content it
// chooses not to touch comes back whole with a nil error.
//
// ⚠ THE SIGNATURE CANNOT CARRY THE REASON, AND THAT IS A REAL LIMIT OF IT. W6.1.1 also calls
// refusal "a valid, LOGGED outcome", and there is nowhere in these four return values to put why.
// So the reason goes to an OBSERVER supplied at construction (see WithObserver) rather than being
// invented into the signature. A caller that needs the reason registers for it; a caller that does
// not is unaffected, and the interface stays the one the item specified.
//
// ⚠ tokensIn/tokensOut ARE ESTIMATES, NOT MEASUREMENTS, and every caller must treat them as such.
// This repo has no tokenizer; `len(x)/4` is the convention it already uses on the pricing path
// (proxy.go, agent_allocator.go). BYTES are what is actually measured here. Reporting an estimate
// as a measurement is the exact failure the holdout in §04 of the design exists to prevent, so the
// two are named differently everywhere in this package.
type Reduction interface {
	Reduce(ctx context.Context, content []byte, kind Kind) (reduced []byte, tokensIn, tokensOut int, err error)
}

// Refusal is what an observer is told when a reducer declines to change the content.
type Refusal struct {
	Kind   Kind
	Bytes  int
	Reason string
}

// Refusal reasons. Closed set: a reason nobody can enumerate cannot be counted, and counting
// refusals by reason is how anyone learns whether this layer is worth having.
const (
	ReasonWrongKind      = "wrong kind for this reducer"
	ReasonNotJSON        = "content is not valid JSON"
	ReasonNoDictArray    = "no array of same-shaped objects to table"
	ReasonNotSmaller     = "the reduced form is not smaller than the input"
	ReasonEmpty          = "empty content"
	ReasonReencodeFailed = "the reduced form failed to re-encode"
)

// EstimateTokens is the repo's existing convention, named for what it is.
//
// ⚠ IT FLOORS, so any content under four bytes estimates ZERO tokens. That is a known asymmetry on
// the pricing path already (see the note on lxcEstimate in internal/proxy) and it is restated here
// so a reader of a Tare saving knows the floor is in the number.
func EstimateTokens(b []byte) int { return len(b) / 4 }
