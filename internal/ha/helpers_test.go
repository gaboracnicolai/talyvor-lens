package ha

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis spins up an in-process miniredis and a client pointed at it,
// matching the idiom used throughout the repo (see internal/cache, ratelimit).
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc, mr
}

// testInstance builds an Instance with sensible defaults for tests.
func testInstance(id, status string, lastSeen time.Time) Instance {
	return Instance{
		ID:        id,
		Host:      "host-" + id,
		StartedAt: lastSeen.Add(-time.Minute),
		LastSeen:  lastSeen,
		Version:   "test",
		Status:    status,
	}
}
