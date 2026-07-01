package proxy

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/cohort"
	"github.com/talyvor/lens/internal/router"
)

type latencyCall struct {
	nodeID, feature, itr, cb, model string
	latencyMs                       int64
	costScore                       int
}

type fakeLatencySink struct{ calls []latencyCall }

func (f *fakeLatencySink) RecordServe(_ context.Context, nodeID, feature, itr, cb, model string, latencyMs int64, costScore int) error {
	f.calls = append(f.calls, latencyCall{nodeID, feature, itr, cb, model, latencyMs, costScore})
	return nil
}

// (proof 3) flag-off ⇒ no capture; nil sink ⇒ no-op (no panic).
func TestCaptureNodeLatency_FlagOffAndNilSink(t *testing.T) {
	fake := &fakeLatencySink{}
	off := &Proxy{nodeLatencySink: fake, nodeLatencyEnabled: func() bool { return false }}
	off.captureNodeLatency("nodeA", "gpt-4o", "chat", "some prompt", 50)
	if len(fake.calls) != 0 {
		t.Fatalf("flag-off must not capture, got %d calls", len(fake.calls))
	}
	// nil sink (flag on) — must no-op, not panic.
	nilSink := &Proxy{nodeLatencyEnabled: func() bool { return true }}
	nilSink.captureNodeLatency("nodeA", "gpt-4o", "chat", "some prompt", 50)
}

// (proof 1, unit) flag-on + limiter available ⇒ one capture with the gateway latency + gateway-derived
// cohort/cost (the node cannot influence these — they're pure functions of the input).
func TestCaptureNodeLatency_CapturesDerivedCohortAndCost(t *testing.T) {
	fake := &fakeLatencySink{}
	p := &Proxy{
		nodeLatencySink:    fake,
		nodeLatencyEnabled: func() bool { return true },
		obsLimiter:         backpressure.New(4),
	}
	const prompt = "write a function that reverses a linked list and prove its complexity"
	p.captureNodeLatency("nodeX", "claude-haiku-4-5", "code", prompt, 137)

	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly one capture, got %d", len(fake.calls))
	}
	c := fake.calls[0]
	wantITR, wantCB := cohort.DeriveInputCohort(prompt)
	wantCost := router.AnalyseComplexity(prompt).Score()
	if c.nodeID != "nodeX" || c.model != "claude-haiku-4-5" || c.feature != "code" || c.latencyMs != 137 {
		t.Errorf("capture identity/model/latency wrong: %+v", c)
	}
	if c.itr != wantITR || c.cb != wantCB || c.costScore != wantCost {
		t.Errorf("derived cohort/cost wrong: got (%s,%s,%d), want (%s,%s,%d) — must equal the gateway pure-derivations",
			c.itr, c.cb, c.costScore, wantITR, wantCB, wantCost)
	}
}

// (proof 2) OFF-PATH SHED: a SATURATED obsLimiter drops the capture without blocking — proving the write
// is off the serve path (a saturated writer never stalls or errors the caller; here the served response
// would already be flushed).
func TestCaptureNodeLatency_ShedsWhenLimiterSaturated(t *testing.T) {
	fake := &fakeLatencySink{}
	lim := backpressure.New(1)
	if !lim.TryAcquire() { // take the only slot so the capture's TryAcquire fails
		t.Fatal("precondition: first acquire must succeed")
	}
	p := &Proxy{nodeLatencySink: fake, nodeLatencyEnabled: func() bool { return true }, obsLimiter: lim}
	p.captureNodeLatency("nodeA", "gpt-4o", "chat", "prompt", 50) // must shed, not block, not call the sink
	if len(fake.calls) != 0 {
		t.Fatalf("saturated limiter must shed the capture, got %d calls", len(fake.calls))
	}
	lim.Release()
}
