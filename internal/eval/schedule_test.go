package eval

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/quality"
)

func TestSchedule_Due(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-1 * time.Minute)

	cases := []struct {
		name string
		s    Schedule
		want bool
	}{
		{"disabled", Schedule{Enabled: false, IntervalSec: 60}, false},
		{"zero interval", Schedule{Enabled: true, IntervalSec: 0}, false},
		{"never run", Schedule{Enabled: true, IntervalSec: 60, LastRunAt: nil}, true},
		{"due (last run old)", Schedule{Enabled: true, IntervalSec: 3600, LastRunAt: &old}, true},
		{"not due (last run recent)", Schedule{Enabled: true, IntervalSec: 3600, LastRunAt: &recent}, false},
	}
	for _, c := range cases {
		if got := c.s.due(now); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

// The scheduler goroutine must exit promptly when its context is cancelled —
// the canonical ticker-loop contract (run under -race).
func TestStartScheduler_StopsOnContextCancel(t *testing.T) {
	p := newPipeline(nil, quality.New(nil), "k", "k", "k")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.StartScheduler(ctx, 10*time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartScheduler did not return after context cancel")
	}
}
