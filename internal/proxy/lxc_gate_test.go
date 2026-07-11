package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/config"
	"github.com/talyvor/lens/internal/workspace"
)

// fakeLXCReader records GetLXCBalance calls and returns a fixed balance/err, so
// the gate helper can be tested across the flag matrix WITHOUT the full serve
// harness. lxcGateBlocks returns a bool; the writeError(402)+return lives at
// the call site (after the budget gate, before the upstream call), so
// "upstream never called" is structural by placement.
type fakeLXCReader struct {
	balance int64
	err     error
	calls   int
}

func (f *fakeLXCReader) GetLXCBalance(_ context.Context, _ string) (int64, error) {
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
	if gateProxy(&fakeLXCReader{balance: est + 1}, true, true).lxcGateBlocks(context.Background(), "wsA", model, prompt, lp) {
		t.Errorf("balance just above the input-only est (%v) must allow", est)
	}
	if !gateProxy(&fakeLXCReader{balance: est - 1}, true, true).lxcGateBlocks(context.Background(), "wsA", model, prompt, lp) {
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

// TestEconomyKillSwitch_LXCGateWorksFiatMode — the U18 fiat reclassification,
// pinned end to end at the gate's decision seam. With the MASTER economy switch
// OFF (LENS_ECONOMY_ENABLED=false) the LXC gate STILL fires, because LXC is fiat:
// its flags survive the kill (config.Load), and those surviving flags — fed
// through the SAME closures main.go installs (SetLXCGate / SetLXCSpendSink) —
// drive a real block/serve decision. Reverting U18's force-off shrink in
// config.Load (re-adding LXC to the kill list) reds this immediately.
//
// Why here, not cmd/lens: lxcGateBlocks (the block/serve seam) is unexported, and
// the established pattern in this file tests the gate at that seam without the
// full serve harness. This IS "the narrowest honest harness over lxcGateBlocks +
// its installation". The PRODUCTION install being present AND unconditional
// (outside any econ guard, like the fiat routes) is pinned separately and
// structurally by TestEconomyKillSwitch_LXCWiringUnconditional in cmd/lens
// (the inverse of WorkersGuarded).
func TestEconomyKillSwitch_LXCGateWorksFiatMode(t *testing.T) {
	// base env config.Load requires (mirrors cmd/lens setRequiredEnv).
	t.Setenv("LENS_REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("LENS_DATABASE_URL", "postgres://localhost:5432/lens")
	t.Setenv("LENS_NATS_URL", "nats://localhost:4222")
	t.Setenv("LENS_OPENAI_API_KEY", "sk-test")
	t.Setenv("LENS_ANTHROPIC_API_KEY", "sk-ant-test")
	// The adversarial fiat setup: MASTER OFF, but LXC (fiat) gating + shadow ON.
	t.Setenv("LENS_ECONOMY_ENABLED", "false")
	t.Setenv("LENS_LXC_GATING_ENABLED", "true")
	t.Setenv("LENS_LXC_SHADOW_SPEND_ENABLED", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Fiat-mode precondition: the master is dead, yet the LXC flags live on.
	if cfg.EconomyEnabled {
		t.Fatal("LENS_ECONOMY_ENABLED=false must kill the master switch")
	}
	if !cfg.LXCGatingEnabled || !cfg.LXCShadowSpendEnabled {
		t.Fatalf("LXC is fiat — its gates must survive the master kill: gating=%v shadow=%v, want both true",
			cfg.LXCGatingEnabled, cfg.LXCShadowSpendEnabled)
	}

	// Install the gate EXACTLY as main.go does: same closures, reading the
	// SURVIVING cfg flags (not hand-set bools) — so a regression that re-kills
	// LXC under the master switch flips these closures to false and reds the
	// block/serve asserts below.
	const costly = "the quick brown fox jumps over the lazy dog repeatedly to accrue tokens"
	gate := func(reader lxcBalanceReader) *Proxy {
		p := &Proxy{}
		p.SetLXCGate(reader, func() bool { return cfg.LXCGatingEnabled })
		p.SetLXCSpendSink(&fakeLXCSink{}, func() bool { return cfg.LXCShadowSpendEnabled })
		return p
	}

	// Zero LXC balance ⇒ REFUSED (economy off, but the fiat gate still bites).
	if !gate(&fakeLXCReader{balance: 0}).lxcGateBlocks(context.Background(), "wsA", "gpt-4o", costly, lp) {
		t.Error("fiat mode (master off, LXC gating on) + zero balance: request must be REFUSED")
	}
	// Positive (ample) balance ⇒ SERVES.
	if gate(&fakeLXCReader{balance: 1e9}).lxcGateBlocks(context.Background(), "wsA", "gpt-4o", costly, lp) {
		t.Error("fiat mode + ample balance: request must SERVE")
	}

	// SENSITIVITY — proves the asserts above exercise the WIRING, not a constant:
	// the SAME flags on a Proxy where SetLXCGate was NOT called never block. Drop
	// the installation and the zero-balance REFUSED above flips → the test reds.
	notInstalled := &Proxy{}
	notInstalled.SetLXCSpendSink(&fakeLXCSink{}, func() bool { return cfg.LXCShadowSpendEnabled })
	if notInstalled.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", costly, lp) {
		t.Error("gate not installed (no SetLXCGate) must never block — confirms the REFUSED above is the installed wiring")
	}
}
