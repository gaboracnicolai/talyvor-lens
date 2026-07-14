package poolroyalty

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeRingDetector struct {
	flags []RingFlag
	err   error
	calls int
}

func (f *fakeRingDetector) DetectSelfDealingRings(context.Context, time.Duration) ([]RingFlag, error) {
	f.calls++
	return f.flags, f.err
}

type fakeAdjudicator struct {
	called bool
	ids    []string
}

func (f *fakeAdjudicator) Adjudicate(_ context.Context, d AdjudicationDecision) (string, RevokeReport, error) {
	f.called = true
	f.ids = d.RevokeRequestIDs
	return "adj-1", RevokeReport{Totals: map[RevokeOutcome]int{OutcomeRevoked: len(d.RevokeRequestIDs)}}, nil
}

// TestAutoAdjudicator_DefaultOff_Inert — a disabled (or nil-gated) adjudicator is
// a TOTAL no-op: it never even runs the detector, and never revokes. Auto-revoke
// must ship inert.
func TestAutoAdjudicator_DefaultOff_Inert(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func() bool
	}{
		{"explicit false", func() bool { return false }},
		{"nil gate", nil},
	} {
		det := &fakeRingDetector{flags: []RingFlag{{RequestID: "r1"}}}
		adj := &fakeAdjudicator{}
		a := NewAutoAdjudicator(det, adj, tc.gate, time.Hour)
		n, err := a.RunOnce(context.Background())
		if err != nil || n != 0 {
			t.Fatalf("%s: n=%d err=%v, want 0/nil", tc.name, n, err)
		}
		if det.calls != 0 {
			t.Errorf("%s: disabled adjudicator must NOT run the detector", tc.name)
		}
		if adj.called {
			t.Errorf("%s: disabled adjudicator must NOT revoke", tc.name)
		}
	}
}

// TestAutoAdjudicator_FailClosed_DetectorError — a detector error aborts the run
// with NO clawback (fail-closed): the error propagates and Adjudicate is never
// called, so nothing is revoked on an ambiguous picture.
func TestAutoAdjudicator_FailClosed_DetectorError(t *testing.T) {
	det := &fakeRingDetector{err: errors.New("detector down")}
	adj := &fakeAdjudicator{}
	a := NewAutoAdjudicator(det, adj, func() bool { return true }, time.Hour)

	n, err := a.RunOnce(context.Background())
	if err == nil {
		t.Fatal("a detector error MUST propagate (fail-closed) — never swallow it into a partial revoke")
	}
	if n != 0 {
		t.Fatalf("n=%d, want 0 on a fail-closed abort", n)
	}
	if adj.called {
		t.Fatal("FAIL-CLOSED violated: Adjudicate must NOT run when detection is ambiguous")
	}
}

// TestAutoAdjudicator_Enabled_RevokesDistinctFlagged — when on and the detector
// flags rings, the distinct flagged request_ids are handed to the durable
// Adjudicate (dedup so the audit set is clean).
func TestAutoAdjudicator_Enabled_RevokesDistinctFlagged(t *testing.T) {
	det := &fakeRingDetector{flags: []RingFlag{
		{RequestID: "r1"}, {RequestID: "r2"}, {RequestID: "r1"}, // r1 duplicated
	}}
	adj := &fakeAdjudicator{}
	a := NewAutoAdjudicator(det, adj, func() bool { return true }, time.Hour)

	n, err := a.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !adj.called {
		t.Fatal("an enabled adjudicator with flagged rings must revoke")
	}
	if len(adj.ids) != 2 {
		t.Fatalf("revoke set = %v, want 2 distinct (r1 deduped)", adj.ids)
	}
	if n != 2 {
		t.Fatalf("revoked=%d, want 2", n)
	}
}

// TestAutoAdjudicator_Enabled_NoFlags_NoAdjudicate — enabled but nothing flagged:
// no clawback call at all (an empty revoke set would error the durable writer).
func TestAutoAdjudicator_Enabled_NoFlags_NoAdjudicate(t *testing.T) {
	det := &fakeRingDetector{flags: nil}
	adj := &fakeAdjudicator{}
	a := NewAutoAdjudicator(det, adj, func() bool { return true }, time.Hour)

	n, err := a.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0/nil", n, err)
	}
	if adj.called {
		t.Error("no flags ⇒ no Adjudicate call")
	}
}
