package economy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// B28.300 — an agent's hourly and weekly caps are judged inside its hold, like its daily and monthly limits: a
// hold over the hourly cap writes no posting, one under it writes one, and what the agent spent before the
// hour (or the week) began does not count.
//
// The rules read "now" from the request's time, so each hold names the time it is judged at — taken from the
// first hold's own posting, so where the hour and the week begin does not depend on when the test runs.
func TestB28300_AHoldOverTheHourlyCapWritesNoPosting_UnderItOne(t *testing.T) {
	pool := supplyPool(t)
	ctx := context.Background()
	s := NewDualTokenStore(nil, pool, nil)
	const ws, key = "ws-caps", "key-caps"
	if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, cache_prefix) VALUES ($1, $1, $1)`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreditLXC(ctx, ws, 100_000_000, "top-up", nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateAgent(ctx, ws, "capped", "user-caps")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachAgentKey(ctx, ws, a.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FundAgent(ctx, ws, a.ID, 50_000_000); err != nil {
		t.Fatal(err)
	}
	setCaps := func(hourly, weekly int64) {
		t.Helper()
		if _, err := s.SetAgentRules(ctx, ws, a.ID, AgentRules{HourlyLimitULXC: &hourly, WeeklyLimitULXC: &weekly}); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(ref string, at time.Time) error {
		return s.ReserveLXCForAgent(WithAgentRequest(ctx, AgentRequest{Model: "gpt-4o", Provider: "openai", At: at}),
			key, ws, ref, 3_000_000, AgentDebitMeta{})
	}
	holds := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_postings WHERE account = $1 AND kind = 'hold'`,
			agentAccount(a.ID)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	refused := func(err error, limit string) bool {
		return errors.Is(err, ErrAgentRule) && strings.Contains(err.Error(), limit+" limit")
	}

	// Hourly cap 5 LXC: a 3 LXC hold is under it and writes one posting; a second, over it, writes none.
	setCaps(5_000_000, 0)
	if err := hold("res-1", time.Time{}); err != nil {
		t.Fatalf("a hold under the hourly cap: %v", err)
	}
	if n := holds(); n != 1 {
		t.Fatalf("a hold under the hourly cap wrote %d hold postings, want 1", n)
	}
	var t1 time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM agent_postings WHERE ref = 'res-1' AND account = $1`,
		agentAccount(a.ID)).Scan(&t1); err != nil {
		t.Fatal(err)
	}
	if err := hold("res-2", t1); !refused(err, "hourly") {
		t.Fatalf("a hold over the hourly cap = %v, want the hourly limit's refusal", err)
	}
	if n := holds(); n != 1 {
		t.Fatalf("a hold over the hourly cap wrote a posting: %d hold postings, want 1", n)
	}
	// The next hour, the first hold no longer counts.
	if err := hold("res-3", t1.Add(time.Hour)); err != nil {
		t.Fatalf("a hold in the next hour: %v", err)
	}

	// Weekly cap 7 LXC, no hourly cap: 6 LXC held this week, so another 3 is over it; next Monday it is not.
	setCaps(0, 7_000_000)
	if err := hold("res-4", t1); !refused(err, "weekly") {
		t.Fatalf("a hold over the weekly cap = %v, want the weekly limit's refusal", err)
	}
	if n := holds(); n != 2 {
		t.Fatalf("a hold over the weekly cap wrote a posting: %d hold postings, want 2", n)
	}
	week := AgentRules{}.periodLimits(t1.UTC(), time.UTC)[2].since
	if err := hold("res-5", week.AddDate(0, 0, 7)); err != nil {
		t.Fatalf("a hold the next week: %v", err)
	}
	if n := holds(); n != 3 {
		t.Fatalf("%d hold postings, want 3", n)
	}
}

// The hour is the clock hour and the week starts on Monday, both in the agent's timezone.
func TestB28300_TheHourAndTheWeekBeginInTheAgentsTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ now, hour, week string }{
		{"2026-10-04 23:30", "2026-10-04 23:00", "2026-09-28 00:00"}, // a Sunday: the week began the Monday before
		{"2026-10-05 00:10", "2026-10-05 00:00", "2026-10-05 00:00"}, // a Monday: the week begins today
		{"2026-11-01 12:59", "2026-11-01 12:00", "2026-10-26 00:00"}, // a Sunday, the week reaching back a month
	} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", tc.now, loc)
		limits := AgentRules{}.periodLimits(now, loc)
		if got := limits[0].since.Format("2006-01-02 15:04"); limits[0].name != "hourly" || got != tc.hour {
			t.Errorf("at %s the %s period began %s, want hourly from %s", tc.now, limits[0].name, got, tc.hour)
		}
		if got := limits[2].since.Format("2006-01-02 15:04"); limits[2].name != "weekly" || got != tc.week {
			t.Errorf("at %s the %s period began %s, want weekly from %s", tc.now, limits[2].name, got, tc.week)
		}
	}
	// 25 Oct 2026 the clocks go back at 04:00 and 03:00–04:00 is lived twice: each hour begins at its own 03:00.
	for _, utc := range []string{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"} {
		now, _ := time.Parse(time.RFC3339, utc)
		want := now.Add(-30 * time.Minute)
		if got := (AgentRules{}).periodLimits(now.In(loc), loc)[0].since; !got.Equal(want) {
			t.Errorf("at %s (%s) the hour began %s, want %s", utc, now.In(loc), got.UTC(), want)
		}
	}
}
