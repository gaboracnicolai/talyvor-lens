package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/workspace"
)

// fakeCaptureSink records RecordPatternObservation calls so the void helper can
// be tested across the flag matrix without the serve harness. The void return
// is the structural safety — its whole observable behavior is which sink calls
// it made.
type fakeCaptureSink struct {
	wsIDs []string
	pats  []mining.RoutingPattern
	err   error
}

func (f *fakeCaptureSink) RecordPatternObservation(_ context.Context, ws string, p mining.RoutingPattern) error {
	f.wsIDs = append(f.wsIDs, ws)
	f.pats = append(f.pats, p)
	return f.err
}

func captureProxy(sink patternCaptureSink, enabled bool) *Proxy {
	p := &Proxy{}
	p.SetPatternCapture(sink, func() bool { return enabled })
	return p
}

// FLAG OFF (default): no sink call (scored=true to isolate the flag gate).
func TestCapturePattern_FlagOff_NoCall(t *testing.T) {
	s := &fakeCaptureSink{}
	p := captureProxy(s, false)
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, false, "")
	if len(s.pats) != 0 {
		t.Fatalf("flag OFF must make ZERO capture calls; got %d", len(s.pats))
	}
}

// FLAG ON + scored: one call, the RoutingPattern built from the args
// (feature/model/provider/quality pass through; ExtractPattern buckets input +
// latency).
func TestCapturePattern_FlagOn_Captures(t *testing.T) {
	s := &fakeCaptureSink{}
	p := captureProxy(s, true)
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, true, "")
	if len(s.pats) != 1 || s.wsIDs[0] != "wsA" {
		t.Fatalf("flag ON must call the sink once for wsA; calls=%d", len(s.pats))
	}
	got := s.pats[0]
	if got.FeatureCategory != "chat" || got.ModelUsed != "gpt-4o" || got.ProviderUsed != "openai" || got.OutputQuality != 0.9 || got.CacheHitRate != 1.0 {
		t.Errorf("pattern not built from args: %+v", got)
	}
}

// NO-POISON: an UNSCORED response (scored=false — e.g. a served non-200
// passthrough, or no scorer wired) must NOT be captured, even with the flag
// on. Capturing it would write output_quality=0 and drag down the Advisor's
// quality average for that model — the exact poison the streaming deferral
// prevents. "Capture only scored responses" is the unifying invariant.
func TestCapturePattern_Unscored_NoCall(t *testing.T) {
	s := &fakeCaptureSink{}
	p := captureProxy(s, true)
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.0, false /*scored*/, 50, false, "")
	if len(s.pats) != 0 {
		t.Fatalf("an UNSCORED response must NOT be captured (no quality=0 poison); calls=%d", len(s.pats))
	}
}

// SERVE UNAFFECTED: a sink error is swallowed; the void helper returns
// normally, nothing propagates (proven by the void return + no panic).
func TestCapturePattern_SinkError_Swallowed(t *testing.T) {
	s := &fakeCaptureSink{err: errors.New("db down")}
	p := captureProxy(s, true)
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, false, "") // must not panic / propagate
	if len(s.pats) != 1 {
		t.Errorf("the capture is attempted then swallowed; calls=%d", len(s.pats))
	}
}

// Nil-safe: nil sink / nil enabled / zero-value Proxy → no call, no panic.
func TestCapturePattern_NilSafe(t *testing.T) {
	(&Proxy{}).capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 1, 1, 0, true, 1, false, "")
	s := &fakeCaptureSink{}
	p := &Proxy{}
	p.SetPatternCapture(s, nil) // nil enabled func
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 1, 1, 0, true, 1, false, "")
	if len(s.pats) != 0 {
		t.Fatalf("nil enabled func must be inert; got %d", len(s.pats))
	}
}

// ─── observational writer bound (#122) ───────────

// A saturated limiter SHEDS the capture — no sink call — and the proxy
// recovers admission once the slot frees.
func TestCapturePattern_LimiterSaturated_Sheds(t *testing.T) {
	s := &fakeCaptureSink{}
	p := captureProxy(s, true)
	l := backpressure.New(1)
	p.SetObservationalLimiter(l)

	if !l.TryAcquire() { // occupy the only slot, as an in-flight writer would
		t.Fatal("setup: could not occupy the slot")
	}
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
	if len(s.pats) != 0 {
		t.Fatalf("saturated limiter must shed the capture; sink calls=%d", len(s.pats))
	}
	if l.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", l.Dropped())
	}

	l.Release()
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
	if len(s.pats) != 1 {
		t.Fatalf("freed limiter must admit again; sink calls=%d", len(s.pats))
	}
}

// A nil limiter (bound disabled / not wired) changes nothing: capture fires.
func TestCapturePattern_NilLimiter_Unbounded(t *testing.T) {
	s := &fakeCaptureSink{}
	p := captureProxy(s, true) // SetObservationalLimiter never called
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
	if len(s.pats) != 1 {
		t.Fatalf("nil limiter must not gate capture; sink calls=%d", len(s.pats))
	}
}

// The detached write context must carry a deadline (#122: detachment must not
// mean "can hang forever" against an exhausted pool).
func TestCapturePattern_WriteContextHasDeadline(t *testing.T) {
	got := make(chan bool, 1)
	s := &deadlineProbeSink{probe: got}
	p := captureProxy(s, true)
	p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
	select {
	case has := <-got:
		if !has {
			t.Fatal("observation write context must have a deadline")
		}
	default:
		t.Fatal("sink was not called")
	}
}

type deadlineProbeSink struct{ probe chan bool }

func (d *deadlineProbeSink) RecordPatternObservation(ctx context.Context, _ string, _ mining.RoutingPattern) error {
	_, has := ctx.Deadline()
	d.probe <- has
	return nil
}

// ── CORPUS SENSITIVITY EXCLUSION (a sensitive request is never captured) ──

// Mirror of the earn exclusion on the mint-free writer: a sensitive request
// (PII, a fired guardrail, or logging==none) must be a NO-OP — RecordPatternObservation
// is NEVER called — even with the flag on and the response scored. The POSITIVE
// CONTROL proves the sink is wired (the byte-identical non-sensitive request IS
// captured). Each term is covered INDEPENDENTLY; logging==none is reachable at
// this function seam even though the proxy.go:1331 call site cannot produce it today.
func TestCapturePattern_SensitiveExcluded(t *testing.T) {
	t.Run("non-sensitive control CAPTURES (proves sink wired)", func(t *testing.T) {
		s := &fakeCaptureSink{}
		p := captureProxy(s, true)
		p.capturePattern(context.Background(), false, false, workspace.LoggingMetadata, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
		if len(s.pats) != 1 {
			t.Fatalf("non-sensitive request must be captured (positive control); calls=%d", len(s.pats))
		}
	})

	for _, c := range []struct {
		name      string
		pii       bool
		guardrail bool
		logging   workspace.LoggingPolicy
	}{
		{"pii-only (guardrail=false, logging!=none)", true, false, workspace.LoggingMetadata},
		{"guardrail-only (pii=false, logging!=none)", false, true, workspace.LoggingFull},
		{"logging==none-only (pii=false, guardrail=false)", false, false, workspace.LoggingNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &fakeCaptureSink{}
			p := captureProxy(s, true)
			p.capturePattern(context.Background(), c.pii, c.guardrail, c.logging, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false, "")
			if len(s.pats) != 0 {
				t.Fatalf("%s: sensitive request must NOT be captured; calls=%d, want 0", c.name, len(s.pats))
			}
		})
	}
}
