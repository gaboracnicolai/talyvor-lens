package proxy

import (
	"context"
	"errors"
	"math"
	"testing"
)

// fakeLXCSink records SpendLXC calls so the shadow-debit helper can be tested
// exhaustively WITHOUT the full serve harness — the helper's void return is the
// structural safety (it can't propagate anything to the response), so its whole
// observable behavior is "which sink calls did it make".
type lxcCall struct {
	ws     string
	amount int64
	desc   string
}

type fakeLXCSink struct {
	calls []lxcCall
	err   error
}

func (f *fakeLXCSink) SpendLXC(_ context.Context, ws string, amount int64, desc string) error {
	f.calls = append(f.calls, lxcCall{ws, amount, desc})
	return f.err
}

func proxyWithSink(sink lxcSpendSink, enabled bool) *Proxy {
	p := &Proxy{}
	p.SetLXCSpendSink(sink, func() bool { return enabled })
	return p
}

// FLAG OFF (default): zero sink calls — the path is fully inert.
func TestShadowSpendLXC_FlagOff_NoCall(t *testing.T) {
	sink := &fakeLXCSink{}
	p := proxyWithSink(sink, false)
	p.shadowSpendLXC(context.Background(), "wsA", 1.0)
	if len(sink.calls) != 0 {
		t.Fatalf("flag OFF must make ZERO sink calls; got %d", len(sink.calls))
	}
}

// FLAG ON, sufficient: sink called once, lxcAmount == costUSD/0.10 (6-dp),
// correct workspace + desc.
func TestShadowSpendLXC_FlagOn_DebitsConverted(t *testing.T) {
	sink := &fakeLXCSink{}
	p := proxyWithSink(sink, true)
	p.shadowSpendLXC(context.Background(), "wsA", 1.0) // $1.00 → 10 LXC
	if len(sink.calls) != 1 {
		t.Fatalf("flag ON must call the sink once; got %d", len(sink.calls))
	}
	c := sink.calls[0]
	if c.ws != "wsA" || c.amount != micro(10) || c.desc != "shadow: AI call billing" {
		t.Errorf("sink call = %+v, want wsA / micro(10) / shadow: AI call billing", c)
	}
}

// 6-dp rounding matches dualtoken's roundTo(_,6).
func TestShadowSpendLXC_SixDecimalRounding(t *testing.T) {
	sink := &fakeLXCSink{}
	p := proxyWithSink(sink, true)
	p.shadowSpendLXC(context.Background(), "wsA", 0.12345678) // /0.10 = 1.2345678 → 1.234568
	if len(sink.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(sink.calls))
	}
	if got, want := sink.calls[0].amount, int64(math.Ceil(0.12345678/0.10*1e6)); got != want || got != 1234568 {
		t.Errorf("lxcAmount = %d, want %d µLXC (ceil of costUSD/0.10)", got, want)
	}
}

// THE MAKE-OR-BREAK SAFETY TEST: flag ON, the sink returns ErrInsufficientLXC.
// The helper must RETURN NORMALLY — no panic, nothing propagated. Because
// shadowSpendLXC returns NOTHING (void), the served response is unaffected by
// construction; a sink error can only be logged-and-swallowed. This is the
// proof that shadow mode cannot break serving.
func TestShadowSpendLXC_InsufficientLXC_Swallowed(t *testing.T) {
	sink := &fakeLXCSink{err: errors.New("economy: insufficient LXC balance")}
	p := proxyWithSink(sink, true)
	// Must not panic and must return (void) — the call was attempted, the error swallowed.
	p.shadowSpendLXC(context.Background(), "wsA", 5.0)
	if len(sink.calls) != 1 {
		t.Fatalf("the debit is attempted (then swallowed); got %d calls", len(sink.calls))
	}
	// The helper returning here at all (no panic, no propagation) is the assertion:
	// a void post-serve call cannot alter the response. Nothing else to check —
	// there is no return value a serve path could branch on.
}

// Sink error of any kind is swallowed identically.
func TestShadowSpendLXC_SinkError_Swallowed(t *testing.T) {
	sink := &fakeLXCSink{err: errors.New("db down")}
	p := proxyWithSink(sink, true)
	p.shadowSpendLXC(context.Background(), "wsA", 2.0) // must not panic / propagate
	if len(sink.calls) != 1 {
		t.Errorf("error must be swallowed after the attempt; calls=%d", len(sink.calls))
	}
}

// costUSD <= 0: no debit (covers cost==0 and the never-negative guard).
func TestShadowSpendLXC_ZeroOrNegativeCost_NoDebit(t *testing.T) {
	sink := &fakeLXCSink{}
	p := proxyWithSink(sink, true)
	p.shadowSpendLXC(context.Background(), "wsA", 0)
	p.shadowSpendLXC(context.Background(), "wsA", -1.5)
	if len(sink.calls) != 0 {
		t.Fatalf("costUSD <= 0 must not debit; got %d calls", len(sink.calls))
	}
}

// Nil sink (not wired) and nil enabled func: no-op, no panic.
func TestShadowSpendLXC_NilSafe(t *testing.T) {
	// nil sink
	p := &Proxy{}
	p.SetLXCSpendSink(nil, func() bool { return true })
	p.shadowSpendLXC(context.Background(), "wsA", 1.0) // no panic
	// sink set but nil enabled func → treated as off
	sink := &fakeLXCSink{}
	p2 := &Proxy{}
	p2.SetLXCSpendSink(sink, nil)
	p2.shadowSpendLXC(context.Background(), "wsA", 1.0)
	if len(sink.calls) != 0 {
		t.Fatalf("nil enabled func must be inert; got %d calls", len(sink.calls))
	}
	// zero-value Proxy (nothing wired) → no panic
	(&Proxy{}).shadowSpendLXC(context.Background(), "wsA", 1.0)
}

// Sub-threshold positive cost that rounds to 0 LXC: no debit, no spurious
// SpendLXC(0) / "debit failed" warning.
// SEC-2: the shadow debit is a CHARGE, so it rounds UP (ceil, house-favoring) —
// a sub-µLXC positive cost now bills the MINIMUM 1 µLXC, never 0. Only a zero /
// non-positive cost skips (costUSD <= 0). This replaces the old round-to-zero
// behavior (which could under-bill a tiny real cost to 0).
func TestShadowSpendLXC_ZeroCost_NoDebit_TinyCostBillsMinimum(t *testing.T) {
	// (a) zero cost → no debit at all.
	sinkZero := &fakeLXCSink{}
	proxyWithSink(sinkZero, true).shadowSpendLXC(context.Background(), "wsA", 0)
	if len(sinkZero.calls) != 0 {
		t.Fatalf("zero cost must not debit; got %d calls", len(sinkZero.calls))
	}
	// (b) a tiny positive cost (< 1 µLXC before ceil) bills exactly 1 µLXC.
	sinkTiny := &fakeLXCSink{}
	proxyWithSink(sinkTiny, true).shadowSpendLXC(context.Background(), "wsA", 0.00000004) // /0.10 = 0.4 µLXC → ceil 1
	if len(sinkTiny.calls) != 1 || sinkTiny.calls[0].amount != 1 {
		t.Fatalf("a tiny positive cost must bill the minimum 1 µLXC (ceil); got %+v", sinkTiny.calls)
	}
}
