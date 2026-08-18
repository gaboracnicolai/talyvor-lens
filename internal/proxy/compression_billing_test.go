package proxy

import (
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// THE BILL DOES NOT MOVE WHEN THE GATE MOVES — MEASURED, NOT READ.
//
// W6.1 handed this down as a caveat to check rather than to trust: "the
// compressor also feeds the pre-serve len/4 estimate, so turning it off slightly
// raises the HOLD." It does not, and the reason is positional. Every seam that
// consumes a TOKEN COUNT reads `prompt`, the ORIGINAL, and every one of them runs
// BEFORE the rewrite exists:
//
//	budget gate           alerts.CostUSD(model, len(prompt)/4, 0)        proxy.go:855
//	LXC gate              p.lxcGateBlocks(..., prompt, ...)              proxy.go:867
//	agent reservation     p.agentReserveBlocks(..., prompt, ...)         proxy.go:885
//	   ── the rewrite is created here ──                                 proxy.go:1239
//	post-serve estimate   inT := len(prompt)/4                           proxy.go:1633
//
// ⚠ THIS TABLE SAID "every MONEY seam" AND THAT WAS FALSE — the entries are the
// seams that read a LENGTH, and one seam reads the rewrite and turns it into a
// PRICE: on the delegated routing path the base router scores compressedPrompt and
// substitutes a cheaper model. The charge claim this file measures survives intact
// (the billed COUNT does not move with the gate); what moves is the RATE. Measured
// and pinned in compression_routing_money_test.go. The five line numbers above are
// re-measured too — four of them had decayed.
//
// Reading that is not the same as measuring it, so this drives the CHARGE end
// through the wire: the same prompt, once with the gate closed and once with it
// wide open, against an upstream that reports NO usage — the only path where the
// len/4 estimate IS the bill. The bytes the provider receives differ; the billed
// input tokens do not.
//
// ⚠ AND IT PINS THE UNCOMFORTABLE HALF OF THE SAME FACT. A workspace that opts IN
// is charged for the bytes it did NOT send: the rewrite shrinks the payload on the
// wire and the invoice still reads the original length. On this path the rewriter
// cannot save its customer a cent no matter how well it compresses. That is not
// repaired here — the item said to note the billing side, not to change it — but
// it is asserted, so a future "compression lowers your bill" claim has to argue
// with a test rather than with a description.
//
// noUsageUpstream omits the usage block entirely, which is what makes
// ExtractUsage report ok=false (internal/inference/usage.go: the *pointer* is
// what distinguishes "absent" from "present but zero").
const noUsageUpstream = `{"content":[{"type":"text","text":"ok"}]}`

// billingProbePrompt is unfenced code with real indentation: the space-run
// collapse shortens it, so "the estimate did not move" is a claim about a case
// where the rewrite genuinely changed the payload's LENGTH. A prompt the rewriter
// leaves alone would make this test pass for the wrong reason.
const billingProbePrompt = "Fix this function:\n" +
	"def normalise(vol):\n" +
	"    if vol.ndim != 3:\n" +
	"        raise ValueError('rank')\n" +
	"    for i in range(vol.shape[0]):\n" +
	"        vol[i] = vol[i] / vol[i].max()\n" +
	"    return vol\n"

func TestBilling_TheEstimateIsTheSameWithTheGateOpenOrClosed(t *testing.T) {
	off, upOff, sinkOff := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-bill-off", Name: "gate closed", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
	}, noUsageUpstream)
	dispatchCompress(t, off, "ws-bill-off", billingProbePrompt, nil)

	on, upOn, sinkOn := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-bill-on", Name: "gate open", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	}, noUsageUpstream)
	dispatchCompress(t, on, "ws-bill-on", billingProbePrompt, nil)

	sentOff, sentOn := upOff.lastPrompt(t), upOn.lastPrompt(t)

	// PREMISE, ASSERTED IN BOTH DIRECTIONS. Without these the equality below is a
	// statement about two identical requests.
	if sentOff != billingProbePrompt {
		t.Fatalf("premise: the closed gate must forward the original; got %q", sentOff)
	}
	if sentOn == billingProbePrompt {
		t.Fatalf("premise: the open gate must forward a REWRITE, otherwise this test compares a request with itself")
	}
	if len(sentOn) >= len(sentOff) {
		t.Fatalf("premise: the rewrite must be SHORTER (%d bytes) than the original (%d) — the whole question is whether a shorter payload bills less", len(sentOn), len(sentOff))
	}

	// PREMISE: the charge must be on the ESTIMATE, not on provider usage.
	if sinkOff.lastEstimated != true || sinkOn.lastEstimated != true {
		t.Fatalf("premise: both rows must be estimated (off=%v on=%v) — with provider usage present this test measures the provider, not the estimate", sinkOff.lastEstimated, sinkOn.lastEstimated)
	}

	// THE MEASUREMENT.
	if sinkOff.lastInput != sinkOn.lastInput {
		t.Errorf("the billed input estimate MOVED with the gate: closed=%d open=%d — every pre-serve money seam is supposed to read the original prompt", sinkOff.lastInput, sinkOn.lastInput)
	}

	// AND WHICH OF THE TWO LENGTHS IT IS. This is the half that is not good news:
	// the opted-in workspace is billed for the original, having sent the rewrite.
	wantOriginal := len(billingProbePrompt) / 4
	if sinkOff.lastInput != wantOriginal {
		t.Errorf("billed input = %d, want len(original)/4 = %d", sinkOff.lastInput, wantOriginal)
	}
	if sentTokens := len(sentOn) / 4; sinkOn.lastInput == sentTokens {
		t.Errorf("the opted-in row billed %d tokens, which equals what was SENT — if the estimate has started following the wire, this test's second claim is stale and the comment above needs rewriting, not deleting", sentTokens)
	}
}
