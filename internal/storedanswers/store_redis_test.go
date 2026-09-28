package storedanswers

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
)

// A workspace's conversions are found by the workspace named beside them: "shared" removes only the
// ones it shared, "all" also its private ones, and another workspace's are never touched.
func TestConversions_DeletedByScope_OnlyTheWorkspaces(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	dc := cache.NewDistillCache(rdb, time.Hour)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(dc.SetWithOwner(ctx, "\x00pool:doc1", "v1", "wsA", []byte("shared by A")))
	must(dc.SetPrivate(ctx, "wsA:doc2", "v1", "wsA", []byte("private to A")))
	must(dc.SetWithOwner(ctx, "\x00pool:doc3", "v1", "wsB", []byte("shared by B")))
	must(dc.SetPrivate(ctx, "wsB:doc4", "v1", "wsB", []byte("private to B")))
	s := New(nil, rdb)
	has := func(hash string) bool {
		b, _ := dc.Get(ctx, hash, "v1")
		return b != nil
	}

	n, err := s.redisMarked(ctx, sharedConversionMarkers, "wsA", true)
	must(err)
	if n != 1 || has("\x00pool:doc1") || !has("wsA:doc2") || !has("\x00pool:doc3") || !has("wsB:doc4") {
		t.Fatalf(`"shared" deleted %d; want exactly A's shared conversion gone and the rest kept`, n)
	}
	n, err = s.redisMarked(ctx, privateConversionMarkers, "wsA", true)
	must(err)
	if n != 1 || has("wsA:doc2") || !has("\x00pool:doc3") || !has("wsB:doc4") {
		t.Fatalf(`"all" deleted %d private; want exactly A's private conversion gone and B's kept`, n)
	}
}
