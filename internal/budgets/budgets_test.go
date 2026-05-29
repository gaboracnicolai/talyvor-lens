package budgets

import (
	"context"
	"sync"
	"testing"
)

// newTestService builds an in-memory Service (no DB) seeded with the given
// budgets and a capturing alert sink. SpentUSD on each budget is the
// starting in-memory total.
func newTestService(bs ...Budget) (*Service, *[]BudgetAlert) {
	s := NewService(nil)
	fired := &[]BudgetAlert{}
	s.alert = func(a BudgetAlert) { *fired = append(*fired, a) }
	s.loadStates(bs)
	return s, fired
}

func wsBudget(limit float64, e Enforcement) Budget {
	return Budget{WorkspaceID: "ws1", Scope: ScopeWorkspace, ScopeID: "ws1", Period: "monthly", LimitUSD: limit, Enforcement: e, AlertThresholds: []float64{0.5, 0.8, 0.9}}
}
func teamBudget(team string, limit float64, e Enforcement) Budget {
	return Budget{WorkspaceID: "ws1", Scope: ScopeTeam, ScopeID: team, Period: "monthly", LimitUSD: limit, Enforcement: e, AlertThresholds: []float64{0.5, 0.8, 0.9}}
}
func sprintBudget(sprint string, limit float64, e Enforcement) Budget {
	return Budget{WorkspaceID: "ws1", Scope: ScopeSprint, ScopeID: sprint, Period: "monthly", LimitUSD: limit, Enforcement: e, AlertThresholds: []float64{0.5, 0.8, 0.9}}
}

func TestCheckBudget_MostRestrictiveWins(t *testing.T) {
	ctx := context.Background()
	// Workspace allows (alert, big limit); team hard-blocks (small limit).
	s, _ := newTestService(
		wsBudget(1000, EnforcementAlert),
		teamBudget("teamA", 10, EnforcementHardBlock),
	)
	if got := s.CheckBudget(ctx, "ws1", "teamA", "", 20); got != DecisionBlock {
		t.Fatalf("team hard_block over limit should win: got %q want block", got)
	}
	// A team with no budget falls through to the (allowing) workspace.
	if got := s.CheckBudget(ctx, "ws1", "teamUnknown", "", 20); got != DecisionAllow {
		t.Fatalf("no team budget should allow via workspace: got %q want allow", got)
	}
	// Sprint hard_block also wins even when workspace+team allow.
	s2, _ := newTestService(
		wsBudget(1000, EnforcementAlert),
		teamBudget("teamA", 1000, EnforcementAlert),
		sprintBudget("sp1", 5, EnforcementHardBlock),
	)
	if got := s2.CheckBudget(ctx, "ws1", "teamA", "sp1", 6); got != DecisionBlock {
		t.Fatalf("sprint hard_block should win: got %q want block", got)
	}
}

func TestCheckBudget_HardBlockRejectsAlertDoesNot(t *testing.T) {
	ctx := context.Background()
	hb, _ := newTestService(wsBudget(10, EnforcementHardBlock))
	if got := hb.CheckBudget(ctx, "ws1", "", "", 11); got != DecisionBlock {
		t.Fatalf("hard_block over limit: got %q want block", got)
	}
	al, _ := newTestService(wsBudget(10, EnforcementAlert))
	if got := al.CheckBudget(ctx, "ws1", "", "", 11); got == DecisionBlock {
		t.Fatalf("alert mode over limit must NOT block: got %q", got)
	}
}

func TestCheckBudget_EnforcementBoundary(t *testing.T) {
	ctx := context.Background()
	// limit 100, already spent 90.
	b := wsBudget(100, EnforcementHardBlock)
	b.SpentUSD = 90
	s, _ := newTestService(b)

	cases := []struct {
		est  float64
		want Decision
	}{
		{9.99, DecisionAllow},  // just under (99.99)
		{10.0, DecisionAllow},  // exactly at limit (100.00) — not over
		{10.01, DecisionBlock}, // just over (100.01)
	}
	for _, c := range cases {
		if got := s.CheckBudget(ctx, "ws1", "", "", c.est); got != c.want {
			t.Fatalf("est=%.2f (projected=%.2f): got %q want %q", c.est, 90+c.est, got, c.want)
		}
	}
}

func TestRecordSpend_ThresholdFiresOncePerThreshold(t *testing.T) {
	ctx := context.Background()
	s, fired := newTestService(wsBudget(100, EnforcementAlert)) // thresholds 0.5/0.8/0.9

	s.RecordSpend(ctx, "ws1", "", "", 60) // ratio 0.60 → crosses 0.5
	s.RecordSpend(ctx, "ws1", "", "", 5)  // ratio 0.65 → no new crossing
	s.RecordSpend(ctx, "ws1", "", "", 30) // ratio 0.95 → crosses 0.8 and 0.9

	if len(*fired) != 3 {
		t.Fatalf("expected 3 threshold alerts, got %d: %+v", len(*fired), *fired)
	}
	seen := map[float64]int{}
	for _, a := range *fired {
		seen[a.Threshold]++
	}
	for _, th := range []float64{0.5, 0.8, 0.9} {
		if seen[th] != 1 {
			t.Fatalf("threshold %.2f fired %d times, want exactly 1", th, seen[th])
		}
	}
	// Further spend past all thresholds must not re-fire.
	s.RecordSpend(ctx, "ws1", "", "", 50) // ratio 1.45
	if len(*fired) != 3 {
		t.Fatalf("thresholds re-fired: got %d alerts, want 3", len(*fired))
	}
}

func TestOffMode_RecordsButNeverBlocksOrAlerts(t *testing.T) {
	ctx := context.Background()
	s, fired := newTestService(wsBudget(10, EnforcementOff))

	if got := s.CheckBudget(ctx, "ws1", "", "", 1000); got != DecisionAllow {
		t.Fatalf("off mode must never block: got %q", got)
	}
	s.RecordSpend(ctx, "ws1", "", "", 100) // 10x the limit

	if len(*fired) != 0 {
		t.Fatalf("off mode must never alert: got %d alerts", len(*fired))
	}
	// But it still tracks spend.
	st := s.Status("ws1", "", "")
	if len(st) != 1 || st[0].SpentUSD != 100 {
		t.Fatalf("off mode should still record spend: %+v", st)
	}
}

func TestRecordSpend_TeamAndSprintAttributionDoesNotBreakWorkspace(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(
		wsBudget(1000, EnforcementAlert),
		teamBudget("teamA", 500, EnforcementAlert),
		sprintBudget("sp1", 50, EnforcementAlert),
		sprintBudget("sp2", 50, EnforcementAlert),
	)
	s.RecordSpend(ctx, "ws1", "teamA", "sp1", 30)
	s.RecordSpend(ctx, "ws1", "teamA", "sp2", 10)
	s.RecordSpend(ctx, "ws1", "", "", 5) // workspace-only traffic

	spent := func(scope Scope, id string) float64 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.states[stateKey("ws1", scope, id)].spent
	}
	if got := spent(ScopeWorkspace, "ws1"); got != 45 {
		t.Fatalf("workspace should accumulate ALL spend: got %.2f want 45", got)
	}
	if got := spent(ScopeTeam, "teamA"); got != 40 {
		t.Fatalf("team should accumulate its own spend: got %.2f want 40", got)
	}
	if got := spent(ScopeSprint, "sp1"); got != 30 {
		t.Fatalf("sprint sp1: got %.2f want 30", got)
	}
	if got := spent(ScopeSprint, "sp2"); got != 10 {
		t.Fatalf("sprint sp2: got %.2f want 10", got)
	}
}

func TestStatus_ReportsUtilizationAndDecision(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(wsBudget(100, EnforcementAlert))
	s.RecordSpend(ctx, "ws1", "", "", 25)

	st := s.Status("ws1", "", "")
	if len(st) != 1 {
		t.Fatalf("expected 1 status row, got %d", len(st))
	}
	if st[0].UtilizationRatio != 0.25 {
		t.Fatalf("utilization: got %.4f want 0.25", st[0].UtilizationRatio)
	}
	if st[0].Decision != DecisionAllow {
		t.Fatalf("decision under limit: got %q want allow", st[0].Decision)
	}

	// Alert-mode budget pushed over the limit reports util>1 and alert.
	s2, _ := newTestService(wsBudget(10, EnforcementAlert))
	s2.RecordSpend(ctx, "ws1", "", "", 15)
	st2 := s2.Status("ws1", "", "")
	if st2[0].UtilizationRatio != 1.5 {
		t.Fatalf("over-limit utilization: got %.4f want 1.5", st2[0].UtilizationRatio)
	}
	if st2[0].Decision != DecisionAlert {
		t.Fatalf("over-limit alert decision: got %q want alert", st2[0].Decision)
	}
}

// TestConcurrentCheckAndRecord exercises the hot path under contention so
// `go test -race` actually has concurrency to inspect. The serialized
// increments must still total exactly, with no data race.
func TestConcurrentCheckAndRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(
		wsBudget(1e9, EnforcementHardBlock),
		teamBudget("teamA", 1e9, EnforcementAlert),
		sprintBudget("sp1", 1e9, EnforcementAlert),
	)
	const goroutines, perG = 50, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				s.CheckBudget(ctx, "ws1", "teamA", "sp1", 1)
				s.RecordSpend(ctx, "ws1", "teamA", "sp1", 1)
				_ = s.Status("ws1", "teamA", "sp1")
			}
		}()
	}
	wg.Wait()

	want := float64(goroutines * perG)
	s.mu.RLock()
	got := s.states[stateKey("ws1", ScopeWorkspace, "ws1")].spent
	s.mu.RUnlock()
	if got != want {
		t.Fatalf("concurrent spend total: got %.0f want %.0f", got, want)
	}
}

func TestCardinalityGuard_BoundsDistinctKeys(t *testing.T) {
	g := newCardinalityGuard(3)
	// First three distinct keys admitted.
	for _, k := range []string{"a", "b", "c"} {
		if !g.allow(k) {
			t.Fatalf("key %q should be admitted within cap", k)
		}
	}
	// A fourth distinct key is refused.
	if g.allow("d") {
		t.Fatal("4th distinct key should be refused by the cardinality guard")
	}
	// Already-seen keys still pass (they emit no new series).
	if !g.allow("a") {
		t.Fatal("already-seen key should still be admitted")
	}
	if g.dropped != 1 {
		t.Fatalf("dropped count: got %d want 1", g.dropped)
	}
	// Many more distinct keys never grow the series set past the cap.
	for i := 0; i < 100; i++ {
		g.allow(string(rune('A' + i)))
	}
	if len(g.seen) != 3 {
		t.Fatalf("series set grew past cap: got %d want 3", len(g.seen))
	}
}
