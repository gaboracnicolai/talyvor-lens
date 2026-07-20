package keel

import (
	"context"
	"testing"
	"time"
)

// Pagination: ListFindingsForWorkspacePage must page with limit+offset (newest-first, no overlap). Without
// OFFSET the two pages would be identical — the unbounded hard-coded-100 read this closes. Rows have no id
// on the read projection, so identity is the distinct first_seen_at we stamp.
func TestListFindingsForWorkspacePage_Paginates(t *testing.T) {
	pool := scopePool(t)
	ctx := context.Background()
	ws := "ws-page"
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, k := range []string{"kp0", "kp1", "kp2", "kp3", "kp4"} {
		seedFinding(t, pool, ws, "idiosyncratic", "ordinary", k)
		if _, err := pool.Exec(ctx,
			`UPDATE keel_findings SET first_seen_at = TIMESTAMPTZ '2026-01-01 00:00:00+00' + make_interval(secs => $2::float8)
			 WHERE identity_key=$1`, k, i); err != nil {
			t.Fatalf("stamp %s: %v", k, err)
		}
	}
	r := NewReader(pool)

	page1, err := r.ListFindingsForWorkspacePage(ctx, ws, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := r.ListFindingsForWorkspacePage(ctx, ws, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("expected 2 per page, got page1=%d page2=%d", len(page1), len(page2))
	}
	off := func(x time.Time) int { return int(x.Sub(base).Seconds()) }
	// newest-first: page1 = seconds {4,3}, page2 = {2,1}
	if off(page1[0].FirstSeenAt) != 4 || off(page1[1].FirstSeenAt) != 3 {
		t.Errorf("page1 wrong: offsets %d,%d want 4,3", off(page1[0].FirstSeenAt), off(page1[1].FirstSeenAt))
	}
	if off(page2[0].FirstSeenAt) != 2 || off(page2[1].FirstSeenAt) != 1 {
		t.Errorf("page2 wrong: offsets %d,%d want 2,1", off(page2[0].FirstSeenAt), off(page2[1].FirstSeenAt))
	}
	seen := map[int]bool{off(page1[0].FirstSeenAt): true, off(page1[1].FirstSeenAt): true}
	for _, f := range page2 {
		if seen[off(f.FirstSeenAt)] {
			t.Fatalf("page 2 overlaps page 1 at offset %d — offset not applied", off(f.FirstSeenAt))
		}
	}
}
