package proxy

import (
	"context"
	"testing"
	"time"
)

// fakeAllowance clamps like billing.Service.Draw: it covers at most what is left.
type fakeAllowance struct{ remaining int64 }

func (f *fakeAllowance) Draw(_ context.Context, _ string, cost int64, _ time.Time) (int64, bool, error) {
	covered := min(cost, f.remaining)
	f.remaining -= covered
	return covered, true, nil
}

func (f *fakeAllowance) RemainingULXC(_ context.Context, _ string, _ time.Time) (int64, bool, error) {
	return f.remaining, true, nil
}

// B1.6 — with prepaid gating on, allowance left counts before prepaid: a subscriber is not
// refused for having no top-up.
func TestLXCGate_AllowanceLeftAdmitsASubscriberWithNoPrepaid(t *testing.T) {
	p := gateProxy(&fakeLXCReader{balance: 0}, true, true)
	p.SetSubscriptionAllowance(&fakeAllowance{remaining: 1e9})
	if p.lxcGateBlocks(context.Background(), "wsA", "gpt-4o", "the quick brown fox jumps over the lazy dog repeatedly to accrue tokens", lp) {
		t.Fatal("a subscriber with allowance left was refused for having no prepaid LXC")
	}
}
