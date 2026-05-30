package mining

import (
	"strings"
	"testing"
)

// TestAggregateCohortsSQL_OptedInOnly is the privacy guard: the routing
// advisor must only ever see patterns from workspaces that opted into
// sharing. The aggregation query that feeds it must filter on opted_in.
func TestAggregateCohortsSQL_OptedInOnly(t *testing.T) {
	if !strings.Contains(aggregateCohortsSQL, "opted_in = TRUE") {
		t.Fatalf("AggregateCohorts must restrict to opted-in patterns; SQL was:\n%s", aggregateCohortsSQL)
	}
	if !strings.Contains(aggregateCohortsSQL, "COUNT(DISTINCT workspace_id)") {
		t.Fatal("AggregateCohorts must report distinct-workspace counts for the multi-workspace floor")
	}
}

func TestInputBucketFor_MatchesRecordedBuckets(t *testing.T) {
	cases := []struct {
		tokens int
		want   string
	}{
		{100, InputBucketSmall},
		{1000, InputBucketMedium},
		{5000, InputBucketLarge},
		{20000, InputBucketXLarge},
	}
	for _, c := range cases {
		if got := InputBucketFor(c.tokens); got != c.want {
			t.Errorf("InputBucketFor(%d) = %q, want %q", c.tokens, got, c.want)
		}
	}
}
