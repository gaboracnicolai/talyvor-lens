package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/worktier"
)

type fakeWorkTierSink struct {
	calls int
	last  worktier.WorkTier
	err   error
}

func (f *fakeWorkTierSink) Record(_ context.Context, _, _, _, _ string, wt worktier.WorkTier,
	_, _ int, _ float64, _ int, _, _ bool) error {
	f.calls++
	f.last = wt
	return f.err
}

func callCapture(p *Proxy) {
	p.captureWorkTier(context.Background(), "ws", "feature", "gpt-4o", "openai",
		"write a function to sort a list step by step", 600, 200, true, false, "metadata")
}

// TestCaptureWorkTier_DefaultOff_NoRecord — flag off ⇒ no classification, no
// Record, zero behavior. The default state.
func TestCaptureWorkTier_DefaultOff_NoRecord(t *testing.T) {
	p := &Proxy{}
	sink := &fakeWorkTierSink{}
	p.SetWorkTier(sink, func() bool { return false })
	callCapture(p)
	if sink.calls != 0 {
		t.Fatalf("flag off must record nothing, got %d Record calls", sink.calls)
	}
}

// TestCaptureWorkTier_Enabled_Records — flag on ⇒ classifies + records once, with
// a non-empty tier (the signals classify deterministically).
func TestCaptureWorkTier_Enabled_Records(t *testing.T) {
	p := &Proxy{}
	sink := &fakeWorkTierSink{}
	p.SetWorkTier(sink, func() bool { return true })
	callCapture(p)
	if sink.calls != 1 {
		t.Fatalf("flag on must record once, got %d", sink.calls)
	}
	// 800 total tokens → small; pii=true,policy=metadata → elevated.
	if sink.last.Size != worktier.SizeSmall || sink.last.Sensitivity != worktier.SensitivityElevated {
		t.Errorf("classified tier wrong: %+v", sink.last)
	}
}

// TestCaptureWorkTier_SinkError_BestEffort — a Record FAILURE is swallowed (void,
// no panic, no propagation). captureWorkTier is post-flush, so this is the
// analogue of the webhook money-safety proof: a classifier/persist failure can
// never affect the already-served response.
func TestCaptureWorkTier_SinkError_BestEffort(t *testing.T) {
	p := &Proxy{}
	sink := &fakeWorkTierSink{err: errors.New("db down")}
	p.SetWorkTier(sink, func() bool { return true })
	callCapture(p) // must not panic and must return normally despite the sink error
	if sink.calls != 1 {
		t.Fatalf("Record should still have been attempted once, got %d", sink.calls)
	}
}

// TestCaptureWorkTier_NilSink_NoOp — no sink wired ⇒ inert.
func TestCaptureWorkTier_NilSink_NoOp(t *testing.T) {
	p := &Proxy{}
	callCapture(p) // must not panic with no sink/flag wired
}
