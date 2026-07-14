package proxy

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/talyvor/lens/internal/routedecision"
)

type fakeRouteSink struct {
	calls int
	last  routedecision.RouteDecision
	err   error
}

func (f *fakeRouteSink) Record(_ context.Context, r routedecision.RouteDecision) error {
	f.calls++
	f.last = r
	return f.err
}

// OFF-PATH: a sink error is swallowed — captureRouteDecision returns normally (the response is already
// flushed; a capture failure must never surface). Proves the descriptive write cannot break serving.
func TestCaptureRouteDecision_OffPath_SwallowsSinkError(t *testing.T) {
	sink := &fakeRouteSink{err: errors.New("db down")}
	p := &Proxy{routeDecisionSink: sink, routeDecisionEnabled: func() bool { return true }}
	p.captureRouteDecision(context.Background(), "ws1", "big", "small", "cohort", true, 5, 100, 50)
	if sink.calls != 1 {
		t.Errorf("sink called %d times, want 1 (error swallowed, not propagated)", sink.calls)
	}
}

// Disabled flag → total no-op (no row).
func TestCaptureRouteDecision_Disabled_NoOp(t *testing.T) {
	sink := &fakeRouteSink{}
	p := &Proxy{routeDecisionSink: sink, routeDecisionEnabled: func() bool { return false }}
	p.captureRouteDecision(context.Background(), "ws1", "big", "small", "cohort", true, 5, 100, 50)
	if sink.calls != 0 {
		t.Errorf("disabled: sink called %d times, want 0", sink.calls)
	}
}

// A missing baseline (not an auto-route decision) → no row.
func TestCaptureRouteDecision_EmptyModels_NoOp(t *testing.T) {
	sink := &fakeRouteSink{}
	p := &Proxy{routeDecisionSink: sink, routeDecisionEnabled: func() bool { return true }}
	p.captureRouteDecision(context.Background(), "ws1", "", "small", "cohort", true, 5, 100, 50)
	if sink.calls != 0 {
		t.Errorf("empty baseline: sink called %d times, want 0", sink.calls)
	}
}

// The override flag is recorded as passed (both branches).
func TestCaptureRouteDecision_RecordsOverrideFlag(t *testing.T) {
	for _, overrode := range []bool{true, false} {
		sink := &fakeRouteSink{}
		p := &Proxy{routeDecisionSink: sink, routeDecisionEnabled: func() bool { return true }}
		p.captureRouteDecision(context.Background(), "ws1", "big", "small", "cohort", overrode, 5, 100, 50)
		if sink.last.CohortOverrode != overrode {
			t.Errorf("recorded CohortOverrode = %v, want %v", sink.last.CohortOverrode, overrode)
		}
	}
}

// Costs are non-negative integer µ-USD; non-finite / non-positive → 0 (no float, no negative amount).
func TestUsdToMicroUSD(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0, 0}, {-1, 0}, {0.000001, 1}, {1.5, 1_500_000},
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
	}
	for _, c := range cases {
		if got := usdToMicroUSD(c.in); got != c.want {
			t.Errorf("usdToMicroUSD(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
