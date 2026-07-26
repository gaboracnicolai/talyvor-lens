package proxy

import (
	"strings"
	"testing"
)

// ⚠ THE THIRD FREE PATH, found by noticing the AST guard's coverage was narrower than its name.
//
// lxcEstimate is the input-only pre-serve LXC estimate. It priced with alerts.CostUSD, which returns 0
// for an unknown model, and TWO live callers read that zero as "nothing to charge → allow":
//
//	agent_allocator.go agentAllocationBlocks  — the immediate-DEBIT path used whenever
//	                                            p.reservationActive() is false (proxy.go:799). On a zero
//	                                            estimate it returns false = serve, WITHOUT DEBITING.
//	                                            LENS_LXC_AGENT_ALLOCATION_ENABLED defaults TRUE.
//	lxc_gate.go        lxcGateBlocks          — the balance ADMISSION gate. A zero estimate can never
//	                                            exceed any balance, so an unknown model is never gated,
//	                                            even on a workspace with no credit.
//
// The reservation seam was fixed first, and it made this one easy to miss: it is the FALLBACK branch, so
// it is invisible whenever reservations are active. Same bug, same file, different function.
//
// The floor (PurposeCharge) is the right direction for BOTH. The debit is immediate and unrefunded, so
// over-charging on a guessed rate is the indefensible outcome; and for the gate, a floor blocks a
// zero-balance workspace without over-blocking a paying one on a number we admit is a guess.
func TestLXCEstimate_UnknownModelIsNeverZero(t *testing.T) {
	prompt := strings.Repeat("token ", 500) // ~3000 chars ⇒ ~750 input tokens

	if got := lxcEstimate(unknownModel, prompt); got <= 0 {
		t.Errorf("lxcEstimate(unknown model) = %d µLXC — a zero here means agentAllocationBlocks serves "+
			"the request with NO DEBIT and the balance gate never blocks it", got)
	}

	// A known model must be unchanged.
	known := lxcEstimate("gpt-4o", prompt)
	if known <= 0 {
		t.Fatalf("lxcEstimate(gpt-4o) = %d, want > 0", known)
	}

	// And the unknown model must price BELOW the expensive known one: the fallback is a floor, so it must
	// never be the more punitive number. This is the asymmetry, asserted rather than assumed.
	expensive := lxcEstimate("claude-fable-5", prompt)
	if got := lxcEstimate(unknownModel, prompt); got > expensive {
		t.Errorf("unknown-model estimate %d exceeds the expensive known model %d — a CHARGE fallback must "+
			"be a floor, never a ceiling", got, expensive)
	}
}

// An EMPTY prompt legitimately estimates 0 — no tokens, no cost. That is not the bug, and the
// `estLXC <= 0` guards must keep allowing it rather than blocking empty requests.
func TestLXCEstimate_EmptyPromptStillZero(t *testing.T) {
	if got := lxcEstimate(unknownModel, ""); got != 0 {
		t.Errorf("lxcEstimate(unknown, \"\") = %d, want 0 — zero TOKENS really is zero cost", got)
	}
}
