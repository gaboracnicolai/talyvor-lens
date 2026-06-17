package proxy

import "github.com/talyvor/lens/internal/workspace"

// sensitiveForCorpus reports whether a just-served request must be EXCLUDED from
// the routing-pattern corpus — from BOTH the mint-capable earn write
// (earnPattern → RecordPattern → credits LENS) and the mint-free capture write
// (capturePattern → RecordPatternObservation). A request is sensitive when it
// carries PII, tripped a (non-blocking) guardrail, or runs under the `none`
// logging policy. Such work opts out of cross-tenant routing intelligence
// entirely: privacy-positive, and on the earn path also mint-excluding — a
// sensitive request never records a pattern, so it never mints.
//
// This is the SINGLE definition of "sensitive for the pattern corpus": both
// writers consult it, so the predicate is never inlined twice and the two can
// never drift apart. (It mirrors decisionTier.sensitive() in
// routing_decision_tier.go, which gates the PRE-serve routing opt-out on the
// same three signals. They are deliberately separate guards on separate call
// paths — pre-serve routing vs post-serve corpus — so a future change to one
// cannot silently move the other.)
//
// loggingPolicy==none is BELT-AND-SUSPENDERS. At today's only call site the
// earn/capture pair already sits inside the alertManager spend/alert block that
// requires loggingPolicy != LoggingNone (proxy.go, ~:1288), so a LoggingNone
// request cannot reach the writers regardless. But that exclusion is INCIDENTAL
// — :1288 is an alerting/spend gate, not an intentional mint-exclusion — and a
// future refactor could move the writers out from under it. This term keeps the
// money-path writers self-protecting if that happens. BOTH guards are
// intentional: neither the :1288 gate nor this term should be removed.
func sensitiveForCorpus(piiDetected, guardrailFired bool, loggingPolicy workspace.LoggingPolicy) bool {
	return piiDetected || guardrailFired || loggingPolicy == workspace.LoggingNone
}
