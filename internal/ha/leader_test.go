package ha

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestLeader_DisabledRunsDirectly verifies that when HA is off, fn runs
// inline with no Redis involvement — single-instance behaviour unchanged.
func TestLeader_DisabledRunsDirectly(t *testing.T) {
	l := NewLeader(nil, "inst-1", false)
	ran := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx, "test-job", 10*time.Second, func(lctx context.Context) {
		close(ran)
		<-lctx.Done()
	})
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("fn did not run within 1s")
	}
}

// TestLeader_CancelStopsFn verifies that cancelling the outer context stops fn.
func TestLeader_CancelStopsFn(t *testing.T) {
	l := NewLeader(nil, "inst-1", false)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go l.Run(ctx, "test-job", 10*time.Second, func(lctx context.Context) {
		<-lctx.Done()
		close(stopped)
	})
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("fn did not stop within 1s after cancel")
	}
}

// TestLeader_FnRunsOnlyOnce verifies that three competing leaders sharing a
// miniredis produce exactly one active fn at a time.
func TestLeader_FnRunsOnlyOnce(t *testing.T) {
	rc, _ := newTestRedis(t)
	var active int32

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, id := range []string{"inst-A", "inst-B", "inst-C"} {
		id := id
		l := NewLeader(rc, id, true)
		go l.Run(ctx, "singleton", 200*time.Millisecond, func(lctx context.Context) {
			if n := atomic.AddInt32(&active, 1); n > 1 {
				t.Errorf("more than one instance active (count=%d, instance=%s)", n, id)
			}
			<-lctx.Done()
			atomic.AddInt32(&active, -1)
		})
	}

	// Let them race for long enough that any double-run would show up.
	time.Sleep(700 * time.Millisecond)
}
