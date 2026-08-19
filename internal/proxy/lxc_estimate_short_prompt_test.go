package proxy

import (
	"context"
	"fmt"
	"testing"
)

// ⚠ THE FOURTH FREE PATH, and it is in the function the third one was fixed in.
//
// lxc_estimate_unknown_model_test.go closed the UNKNOWN-MODEL zero: lxcEstimate now prices an
// unrecognised model on the derived floor, so it can no longer return 0 for one. Both of its callers
// still read a zero as permission, and both now say in prose that the only thing left down there is an
// empty prompt:
//
//	agent_allocator.go  "Empty prompt only: zero TOKENS is genuinely zero cost."
//	lxc_gate.go         "An EMPTY prompt still yields 0, correctly ... so the callers' `<= 0`
//	                     branches remain reachable for that case and only that case."
//
// ⚠ BOTH SENTENCES ARE FALSE, AND THE REASON IS ONE INTEGER DIVISION. lxcEstimate converts bytes to
// tokens with `len(prompt)/4`, which FLOORS. A prompt of 1, 2 or 3 bytes is therefore ZERO input
// tokens, prices at zero, and takes the same free branch the empty prompt takes — on ANY model,
// known or unknown. The function's own docstring says it "rounds UP (ceil): a conservative estimate
// never under-reserves a sub-µLXC", and it does — but only at the µLXC conversion. The token
// conversion one line earlier rounds DOWN, and that is the half the free path comes out of.
//
// The previous guard could not see it: it tests exactly two lengths, 0 and ~3000, so the whole
// interval where the floor bites was never sampled.
//
// WHAT IT COSTS is measured in TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt
// below — not reasoned about here.
//
// ⚠ NOT FIXED, AND THAT IS DELIBERATE. Every repair moves a number on a live money path:
// ceil-ing the token count re-prices EVERY prompt whose length is not a multiple of 4; debiting a
// 1 µLXC floor invents a charge; blocking short prompts at an exhausted ceiling refuses traffic that
// is served today. Which of those is right is a pricing/product decision, not a session's. This file
// PINS the measured boundary so the free path cannot silently widen, and so whoever takes the
// decision has to edit a test that states what they are changing.
func TestLXCEstimate_TheFreePathIsShorterThanFourBytesNotJustEmpty(t *testing.T) {
	// Both models: the fix for the unknown-model zero does not reach this one, because the floor is
	// applied to a token count that is already 0.
	for _, model := range []string{"gpt-4o", unknownModel} {
		t.Run(model, func(t *testing.T) {
			// 0..3 bytes ⇒ 0 input tokens ⇒ 0 µLXC ⇒ the `<= 0` free branch.
			for _, p := range []string{"", "y", "go", "ok?"} {
				if got := lxcEstimate(model, p); got != 0 {
					t.Errorf("lxcEstimate(%q, %q) = %d, want 0 — this is the measured free path "+
						"(len(prompt)/4 floors to 0 tokens); if this now prices, the token "+
						"conversion changed and BOTH callers' comments plus this file must be "+
						"rewritten to say so", model, p, got)
				}
			}
			// 4 bytes is the first length that prices. This is the boundary, asserted from BOTH
			// sides so a change in either direction reds rather than only a widening.
			for _, p := range []string{"yes!", "okay!", "four score and seven"} {
				if got := lxcEstimate(model, p); got <= 0 {
					t.Errorf("lxcEstimate(%q, %q) = %d, want > 0 — 4 bytes is one input token and "+
						"must be charged; a zero here would widen the free path", model, p, got)
				}
			}
		})
	}
}

// THE SECOND CALLER, pinned so lxc_gate.go's corrected comment is a measurement rather than an
// inference. lxcGateBlocks is the balance ADMISSION gate: a zero estimate can never exceed any
// balance, so a zero-credit workspace is admitted for a sub-4-byte prompt. Same positive control as
// the real-PG test below — the ordinary prompt must BLOCK on the same zero balance, or "it was
// admitted" would just mean the gate was off.
func TestLXCGateBlocks_ASubFourBytePromptIsAdmittedOnAZeroBalance(t *testing.T) {
	const ordinary = "a prompt long enough that the input-token estimate is a clean positive number"

	// CONTROL: zero balance + a normal prompt ⇒ blocked. Fixture is live.
	if !gateProxy(&fakeLXCReader{balance: 0}, true, true).
		lxcGateBlocks(context.Background(), "wsA", "gpt-4o", ordinary, lp) {
		t.Fatalf("CONTROL FAILED: a zero balance must BLOCK a %d-byte prompt (est %d µLXC); "+
			"the admission below would otherwise prove nothing", len(ordinary), lxcEstimate("gpt-4o", ordinary))
	}

	for _, short := range []string{"ok?", "go", "y"} {
		r := &fakeLXCReader{balance: 0}
		if gateProxy(r, true, true).lxcGateBlocks(context.Background(), "wsA", "gpt-4o", short, lp) {
			t.Fatalf("MEASURED BEHAVIOUR CHANGED: a %d-byte prompt is now gated on a zero balance — "+
				"update this test and lxc_gate.go's comment deliberately", len(short))
		}
		// It does not even read the balance: the estimate short-circuits above the reader.
		if r.calls != 0 {
			t.Errorf("the zero-estimate branch read the balance %d time(s) — it is documented as "+
				"returning before the read", r.calls)
		}
	}
}

// THE COST OF THE FREE PATH, THROUGH REAL POSTGRES AND THE REAL CEILING.
//
// agentAllocationBlocks documents itself as an "airtight ceiling": "the serve path is entered IFF a
// debit was booked". This measures that claim against an agent whose sub-budget is FULLY EXHAUSTED
// (remaining = 0) and finds it true for an ordinary prompt and false for a three-byte one.
//
// ⚠ THE BLOCKED CASE IS NOT DECORATION — IT IS THE POSITIVE CONTROL. An "it was served" assertion
// alone would pass just as happily against a ceiling that was never wired, a spender that was nil, or
// a flag that was off. Proving in the SAME test, against the SAME exhausted row, that a normal prompt
// IS refused is what makes the short-prompt pass evidence of a hole rather than evidence of an inert
// fixture.
//
// ⚠ AND IT ASSERTS ON lxc_spend_claims, NOT ON A RETURN VALUE. `blocked == false` says the request was
// allowed; it does not say no money moved. The claim row is written inside SpendLXCForAgent's single
// transaction, so its ABSENCE is the durable proof that the debit path was never entered — the request
// was not "debited zero", it was not billed at all.
func TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt(t *testing.T) {
	p, _, pool := agentAllocHarness(t)
	ctx := context.Background()
	const key = "agent-short-prompt-freepath"
	const ws = "ws-short-prompt-freepath"

	// A funded workspace — so nothing below can be explained away as an empty balance — whose agent
	// has spent its entire sub-budget.
	if _, err := pool.Exec(ctx,
		`INSERT INTO lxc_balances (workspace_id, balance) VALUES ($1, 100000000)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, 1000, 1000)`, key, ws); err != nil {
		t.Fatal(err)
	}
	var ceiling, spent int64
	if err := pool.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1`,
		key).Scan(&ceiling, &spent); err != nil {
		t.Fatal(err)
	}
	if ceiling-spent != 0 {
		t.Fatalf("fixture: sub-budget must be exhausted, remaining = %d", ceiling-spent)
	}

	claims := func() int {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM lxc_spend_claims WHERE scoped_key_id = $1`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// POSITIVE CONTROL: the ceiling is live and refuses an ordinary prompt on this very row.
	const ordinary = "a prompt long enough that the input-token estimate is a clean positive number"
	if !p.agentAllocationBlocks(ctx, key, ws, agentTestModel, ordinary, "req-ordinary") {
		t.Fatalf("CONTROL FAILED: an exhausted sub-budget (ceiling=%d spent=%d) must BLOCK a %d-byte "+
			"prompt (est %d µLXC). Everything below is meaningless if this does not hold — it would "+
			"mean the ceiling is inert rather than bypassed.",
			ceiling, spent, len(ordinary), lxcEstimate(agentTestModel, ordinary))
	}
	if n := claims(); n != 0 {
		t.Fatalf("a blocked request must leave no claim row (the tx rolls back); got %d", n)
	}

	// THE FREE PATH: same exhausted agent, same model, same flag — a shorter prompt is served, and
	// no debit is even attempted.
	for _, short := range []string{"ok?", "go", "y"} {
		req := fmt.Sprintf("req-short-%d", len(short))
		blocked := p.agentAllocationBlocks(ctx, key, ws, agentTestModel, short, req)
		if blocked {
			t.Fatalf("MEASURED BEHAVIOUR CHANGED: a %d-byte prompt is now BLOCKED at an exhausted "+
				"ceiling. That is very likely the intended repair — but it re-prices or refuses "+
				"traffic that was served, so update this test and both callers' comments "+
				"deliberately rather than deleting the assertion.", len(short))
		}
		if n := claims(); n != 0 {
			t.Fatalf("a %d-byte prompt booked a claim row (%d) — the debit path was entered after "+
				"all; the free path this file documents no longer exists", len(short), n)
		}
	}

	// And the ceiling row is untouched: nothing was spent by any of it.
	var spentAfter int64
	if err := pool.QueryRow(ctx,
		`SELECT spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1`, key).Scan(&spentAfter); err != nil {
		t.Fatal(err)
	}
	if spentAfter != spent {
		t.Errorf("spent_lxc moved %d -> %d across requests that booked no claim", spent, spentAfter)
	}
}
