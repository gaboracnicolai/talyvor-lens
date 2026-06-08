package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// fakeLXCReader records GetLXCBalance calls and returns a fixed balance/err, so
// the gate helper can be tested across the flag matrix WITHOUT the full serve
// harness. lxcGateBlocks returns a bool; the writeError(402)+return lives at
// the call site (after the budget gate, before the upstream call), so
// "upstream never called" is structural by placement.
type fakeLXCReader struct {
	balance float64
	err     error
	calls   int
}

func (f *fakeLXCReader) GetLXCBalance(_ context.Context, _ string) (float64, error) {
	f.calls++
	return f.balance, f.err
}

// lp is the default non-None logging policy for the live-path tests (the gate
// is inert for LoggingNone — exercised separately).
const lp = workspace.LoggingMetadata

// gateProxy wires the gate reader + both flag closures (gating, shadow).
func gateProxy(reader lxcBalanceReader, gating, shadow bool) *Proxy {
	p := &Proxy{}
	p.SetLXCGate(reader, func() bool { return gating })
	p.SetLXCSpendSink(&fakeLXCSink{}, func() bool { return shadow }) // gate reads lxcShadowEnabled for coherence
	return p
}

// FLAG OFF (default): the gate never blocks and never reads the balance.
func TestLXCGateBlocks_FlagOff_NeverBlocksNoRead(t *testing.T) {
	r := &fakeLXCReader{balance: 0} // would block if consulted
	p := gateProxy(r, false /*gating*/, false /*shadow*/)
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "a long prompt that costs something", lp) {
		t.Fatal("flag OFF must never block")
	}
	if r.calls != 0 {
		t.Fatalf("flag OFF must not read the balance; reads=%d", r.calls)
	}
}

// COHERENCE — gating ON + shadow OFF: INERT (no block, no read). The interlock
// that prevents block-without-accounting.
func TestLXCGateBlocks_GatingOnShadowOff_Inert(t *testing.T) {
	r := &fakeLXCReader{balance: 0}
	p := gateProxy(r, true /*gating*/, false /*shadow*/)
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "a long prompt", lp) {
		t.Fatal("gating ON + shadow OFF must NOT block (coherence)")
	}
	if r.calls != 0 {
		t.Fatalf("inert config must not read the balance; reads=%d", r.calls)
	}
}

// COHERENCE (per-request) — gating+shadow ON but LoggingNone: INERT. The shadow
// debit never fires for LoggingNone, so the gate must not block it (else the
// balance freezes and the workspace blocks forever, spending nothing). The
// gate's live-condition must match the debit's fire-condition exactly.
func TestLXCGateBlocks_LoggingNone_Inert(t *testing.T) {
	r := &fakeLXCReader{balance: 0} // would block if consulted
	p := gateProxy(r, true /*gating*/, true /*shadow*/)
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "a long prompt", workspace.LoggingNone) {
		t.Fatal("LoggingNone must NOT block — the debit never fires for it (no block without accounting)")
	}
	if r.calls != 0 {
		t.Fatalf("LoggingNone must not read the balance; reads=%d", r.calls)
	}
}

// INSUFFICIENT LXC (make-or-break): gating+shadow ON, non-None policy,
// balance < estLXC → BLOCK.
func TestLXCGateBlocks_InsufficientLXC_Blocks(t *testing.T) {
	r := &fakeLXCReader{balance: 0}
	p := gateProxy(r, true, true)
	if !p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "the quick brown fox jumps over the lazy dog repeatedly to accrue tokens", lp) {
		t.Fatal("insufficient LXC must BLOCK (the first gate that changes whether a request succeeds)")
	}
	if r.calls != 1 {
		t.Fatalf("the gate must read the balance exactly once; reads=%d", r.calls)
	}
}

// SUFFICIENT LXC → ALLOW.
func TestLXCGateBlocks_SufficientLXC_Allows(t *testing.T) {
	r := &fakeLXCReader{balance: 1e9}
	p := gateProxy(r, true, true)
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "short prompt", lp) {
		t.Fatal("sufficient LXC must ALLOW")
	}
}

// FAIL-OPEN: a balance-read error → ALLOW, mirroring the spend cap.
func TestLXCGateBlocks_ReadError_FailsOpen(t *testing.T) {
	r := &fakeLXCReader{balance: 0, err: errors.New("db down")}
	p := gateProxy(r, true, true)
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "a long prompt that would otherwise block", lp) {
		t.Fatal("a balance-read error must FAIL OPEN (allow), not block")
	}
}

// EXACT-EQUAL boundary: balance == estLXC → ALLOW (strict <, matching the
// budget gate's "strictly over blocks" convention — no off-by-one).
func TestLXCGateBlocks_BalanceEqualsEstimate_Allows(t *testing.T) {
	const model = "gpt-4o"
	prompt := "boundary prompt for the exact-equal balance case"
	est := lxcEstimate(model, prompt)
	if est <= 0 {
		t.Skip("model not in catalog → est 0")
	}
	p := gateProxy(&fakeLXCReader{balance: est}, true, true)
	if p.lxcGateBlocks(context.Background(), "wsA", model, prompt, lp) {
		t.Errorf("balance == estLXC (%v) must ALLOW (strict <, allow-at-equal)", est)
	}
}

// ESTIMATE IS INPUT-ONLY (output=0), under-blocks by design: just-above allows,
// just-below blocks.
func TestLXCGateBlocks_InputOnlyEstimate(t *testing.T) {
	const model = "gpt-4o"
	prompt := "estimate me with input tokens only"
	est := lxcEstimate(model, prompt)
	if est <= 0 {
		t.Skip("model not in catalog → est 0; block/allow shape covered elsewhere")
	}
	if gateProxy(&fakeLXCReader{balance: est + 0.000001}, true, true).lxcGateBlocks(context.Background(), "wsA", model, prompt, lp) {
		t.Errorf("balance just above the input-only est (%v) must allow", est)
	}
	if !gateProxy(&fakeLXCReader{balance: est - 0.000001}, true, true).lxcGateBlocks(context.Background(), "wsA", model, prompt, lp) {
		t.Errorf("balance just below the input-only est (%v) must block", est)
	}
}

// Nil-safe: no reader / nil flags → never blocks, no panic.
func TestLXCGateBlocks_NilSafe(t *testing.T) {
	if (&Proxy{}).lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "p", lp) {
		t.Fatal("zero-value Proxy must not block")
	}
	p := &Proxy{}
	p.SetLXCGate(nil, func() bool { return true }) // nil reader, gating on, shadow unset
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "p", lp) {
		t.Fatal("nil reader must not block")
	}
}
