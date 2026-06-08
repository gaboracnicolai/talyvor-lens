package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/mining"
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
	p.capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, false)
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
	p.capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, true)
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
	p.capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.0, false /*scored*/, 50, false)
	if len(s.pats) != 0 {
		t.Fatalf("an UNSCORED response must NOT be captured (no quality=0 poison); calls=%d", len(s.pats))
	}
}

// SERVE UNAFFECTED: a sink error is swallowed; the void helper returns
// normally, nothing propagates (proven by the void return + no panic).
func TestCapturePattern_SinkError_Swallowed(t *testing.T) {
	s := &fakeCaptureSink{err: errors.New("db down")}
	p := captureProxy(s, true)
	p.capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true /*scored*/, 50, false) // must not panic / propagate
	if len(s.pats) != 1 {
		t.Errorf("the capture is attempted then swallowed; calls=%d", len(s.pats))
	}
}

// Nil-safe: nil sink / nil enabled / zero-value Proxy → no call, no panic.
func TestCapturePattern_NilSafe(t *testing.T) {
	(&Proxy{}).capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 1, 1, 0, true, 1, false)
	s := &fakeCaptureSink{}
	p := &Proxy{}
	p.SetPatternCapture(s, nil) // nil enabled func
	p.capturePattern(context.Background(), "wsA", "chat", "gpt-4o", "openai", 1, 1, 0, true, 1, false)
	if len(s.pats) != 0 {
		t.Fatalf("nil enabled func must be inert; got %d", len(s.pats))
	}
}
