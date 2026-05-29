package forecast

import (
	"context"
	"go/build"
	"math"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/budgets"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// ─── mock source ───

type mockSource struct {
	spent       float64
	buckets     []DayBucket
	budgets     []budgets.Budget
	spentCalls  int
	bucketCalls int
	budgetCalls int
}

func (m *mockSource) PeriodSpent(_ context.Context, _ budgets.Budget) (float64, error) {
	m.spentCalls++
	return m.spent, nil
}
func (m *mockSource) DailyBuckets(_ context.Context, _ string, _ budgets.Scope, _ string, _ int, _ time.Time) ([]DayBucket, error) {
	m.bucketCalls++
	return m.buckets, nil
}
func (m *mockSource) Budgets(_ context.Context, _ string) ([]budgets.Budget, error) {
	m.budgetCalls++
	return m.budgets, nil
}

// dailyBuckets builds one $perDay bucket for each of the `days` days ending
// the day before `end` (i.e. [end-days, end)).
func dailyBuckets(end time.Time, days int, perDay float64) []DayBucket {
	var out []DayBucket
	for i := days; i >= 1; i-- {
		day := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -i)
		out = append(out, DayBucket{Day: day, SpendUSD: perDay})
	}
	return out
}

func newForecasterAt(m *mockSource, now time.Time) *Forecaster {
	f := New(m)
	f.now = func() time.Time { return now }
	return f
}

// ─── pure math ───

func TestLinearRunRate_HalfPeriodDoublesProjection(t *testing.T) {
	if got := linearRunRate(500, 0.5); !approx(got, 1000) {
		t.Fatalf("half period, $500 spent → got %.4f want 1000", got)
	}
	if got := linearRunRate(300, 0.25); !approx(got, 1200) {
		t.Fatalf("quarter period, $300 spent → got %.4f want 1200", got)
	}
	if got := linearRunRate(100, 0); got != 0 {
		t.Fatalf("zero elapsed must not divide-by-zero: got %.4f", got)
	}
}

func TestTrailingWindow_RateAndProjection(t *testing.T) {
	// $700 over 7 days = $100/day; 10 days remaining + $400 spent → 400 + 1000 = 1400.
	projected, rate := trailingWindow(700, 7, 400, 10)
	if !approx(rate, 100) {
		t.Fatalf("daily rate: got %.4f want 100", rate)
	}
	if !approx(projected, 1400) {
		t.Fatalf("projection: got %.4f want 1400", projected)
	}
}

func TestElapsedFraction(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 10)
	if got := elapsedFraction(start, end, start.AddDate(0, 0, 5)); !approx(got, 0.5) {
		t.Fatalf("mid: got %.4f want 0.5", got)
	}
	if got := elapsedFraction(start, end, start.AddDate(0, 0, -1)); got != 0 {
		t.Fatalf("before start must clamp to 0: got %.4f", got)
	}
	if got := elapsedFraction(start, end, end.AddDate(0, 0, 5)); got != 1 {
		t.Fatalf("after end must clamp to 1: got %.4f", got)
	}
}

func TestTrend_Labels(t *testing.T) {
	if got := trend(120, 100); got != TrendAccelerating {
		t.Fatalf("120 vs 100: got %q want accelerating", got)
	}
	if got := trend(105, 100); got != TrendSteady {
		t.Fatalf("105 vs 100 (within band): got %q want steady", got)
	}
	if got := trend(80, 100); got != TrendDecelerating {
		t.Fatalf("80 vs 100: got %q want decelerating", got)
	}
	if got := trend(50, 0); got != TrendAccelerating {
		t.Fatalf("from zero to positive: got %q want accelerating", got)
	}
}

func TestConfidenceNote_ReflectsDataVolume(t *testing.T) {
	if note := confidenceNote(3); !contains(note, "low confidence") {
		t.Fatalf("3 days should be low confidence: %q", note)
	}
	if note := confidenceNote(21); !contains(note, "reasonable") {
		t.Fatalf("21 days should read reasonable: %q", note)
	}
}

func TestExhaustionDate(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	// limit 1000, spent 400, $100/day → 6 days to exhaust.
	got, ok := exhaustionDate(1000, 400, 100, now)
	if !ok {
		t.Fatal("expected an exhaustion date")
	}
	want := now.AddDate(0, 0, 6)
	if got.Sub(want).Abs() > time.Minute {
		t.Fatalf("exhaustion: got %v want ~%v", got, want)
	}
	// Zero rate → never exhausts.
	if _, ok := exhaustionDate(1000, 400, 0, now); ok {
		t.Fatal("zero daily rate should not produce an exhaustion date")
	}
	// Already over → now.
	if d, ok := exhaustionDate(1000, 1200, 50, now); !ok || !d.Equal(now) {
		t.Fatalf("already-exhausted should return now: got %v ok=%v", d, ok)
	}
}

// ─── orchestration: vs_budget ───

func TestProjectScope_WillExceedWhenProjectionOverLimit(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC) // monthly: 15/31 elapsed
	m := &mockSource{
		spent:   600, // 600 * 31/15 = $1240 projected
		buckets: dailyBuckets(now, 10, 50),
		budgets: []budgets.Budget{{WorkspaceID: "ws1", Scope: budgets.ScopeWorkspace, ScopeID: "ws1", Period: "monthly", LimitUSD: 1000}},
	}
	f := newForecasterAt(m, now)
	fc, err := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "", "monthly")
	if err != nil {
		t.Fatal(err)
	}
	if fc.Method != MethodLinearRunRate {
		t.Fatalf("method: got %q want linear_run_rate", fc.Method)
	}
	if !approx(fc.ProjectedTotalUSD, 1240) {
		t.Fatalf("projected: got %.2f want 1240", fc.ProjectedTotalUSD)
	}
	if fc.VsBudget == nil || !fc.VsBudget.WillExceed {
		t.Fatalf("expected will_exceed=true, got %+v", fc.VsBudget)
	}
	if !approx(fc.VsBudget.ProjectedOverageUSD, 240) {
		t.Fatalf("overage: got %.2f want 240", fc.VsBudget.ProjectedOverageUSD)
	}
}

func TestProjectScope_WillNotExceedWhenUnderLimit(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	m := &mockSource{
		spent:   300, // 300 * 31/15 = $620 projected
		buckets: dailyBuckets(now, 10, 30),
		budgets: []budgets.Budget{{WorkspaceID: "ws1", Scope: budgets.ScopeWorkspace, ScopeID: "ws1", Period: "monthly", LimitUSD: 1000}},
	}
	f := newForecasterAt(m, now)
	fc, _ := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if fc.VsBudget == nil || fc.VsBudget.WillExceed {
		t.Fatalf("expected will_exceed=false, got %+v", fc.VsBudget)
	}
	if fc.VsBudget.ProjectedOverageUSD != 0 {
		t.Fatalf("overage should be 0 under limit: got %.2f", fc.VsBudget.ProjectedOverageUSD)
	}
}

func TestProjectScope_ExhaustionDateFromDailyRate(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	m := &mockSource{
		spent:   400,
		buckets: dailyBuckets(now, 7, 100), // last 7 days @ $100 → $100/day
		budgets: []budgets.Budget{{WorkspaceID: "ws1", Scope: budgets.ScopeWorkspace, ScopeID: "ws1", Period: "monthly", LimitUSD: 1000}},
	}
	f := newForecasterAt(m, now)
	fc, _ := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if !approx(fc.DailyRateUSD, 100) {
		t.Fatalf("daily rate: got %.2f want 100", fc.DailyRateUSD)
	}
	if fc.VsBudget == nil || fc.VsBudget.EstExhaustionDate == nil {
		t.Fatal("expected an exhaustion date")
	}
	want := now.AddDate(0, 0, 6) // (1000-400)/100 = 6 days
	if fc.VsBudget.EstExhaustionDate.Sub(want).Abs() > time.Minute {
		t.Fatalf("exhaustion: got %v want ~%v", *fc.VsBudget.EstExhaustionDate, want)
	}
}

// ─── insufficient data ───

func TestProjectScope_InsufficientWhenFreshPeriod(t *testing.T) {
	now := time.Date(2026, 5, 1, 6, 0, 0, 0, time.UTC) // 6h into the month
	m := &mockSource{
		spent:   25,
		buckets: dailyBuckets(now, 1, 25),
		budgets: []budgets.Budget{{WorkspaceID: "ws1", Scope: budgets.ScopeWorkspace, ScopeID: "ws1", Period: "monthly", LimitUSD: 1000}},
	}
	f := newForecasterAt(m, now)
	fc, _ := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if !fc.InsufficientData || fc.Method != MethodInsufficient {
		t.Fatalf("fresh period must be insufficient: %+v", fc)
	}
	if fc.ProjectedTotalUSD != 0 {
		t.Fatalf("insufficient must not extrapolate: projected=%.2f", fc.ProjectedTotalUSD)
	}
	if fc.VsBudget == nil || fc.VsBudget.WillExceed {
		t.Fatalf("insufficient must not claim will_exceed: %+v", fc.VsBudget)
	}
}

func TestProjectScope_InsufficientWhenFewerThanTwoDays(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC) // plenty elapsed
	m := &mockSource{
		spent:   80,
		buckets: dailyBuckets(now, 1, 80), // only ONE day with spend
		budgets: nil,
	}
	f := newForecasterAt(m, now)
	fc, _ := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if !fc.InsufficientData {
		t.Fatalf("one day of data must be insufficient: dataDays=%d %+v", fc.DataDays, fc)
	}
	if !contains(fc.ConfidenceNote, "insufficient") {
		t.Fatalf("note should explain insufficiency: %q", fc.ConfidenceNote)
	}
}

func TestProjectScope_SufficientHasConfidenceNote(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	m := &mockSource{spent: 300, buckets: dailyBuckets(now, 10, 30)}
	f := newForecasterAt(m, now)
	fc, _ := f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if fc.InsufficientData {
		t.Fatalf("10 days should be sufficient: %+v", fc)
	}
	if fc.ConfidenceNote == "" || contains(fc.ConfidenceNote, "insufficient") {
		t.Fatalf("expected a confidence note: %q", fc.ConfidenceNote)
	}
}

// ─── cache ───

func TestProjectScope_CachesWithinTTL(t *testing.T) {
	now := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	m := &mockSource{spent: 300, buckets: dailyBuckets(now, 10, 30)}
	clock := now
	f := New(m)
	f.now = func() time.Time { return clock }

	_, _ = f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	_, _ = f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if m.spentCalls != 1 || m.bucketCalls != 1 {
		t.Fatalf("within TTL the store must be queried once: spentCalls=%d bucketCalls=%d", m.spentCalls, m.bucketCalls)
	}

	// Advance past the TTL → recompute.
	clock = now.Add(defaultTTL + time.Second)
	_, _ = f.ProjectScope(context.Background(), "ws1", budgets.ScopeWorkspace, "ws1", "monthly")
	if m.spentCalls != 2 {
		t.Fatalf("after TTL expiry the store must be re-queried: spentCalls=%d", m.spentCalls)
	}
}

// ─── structural: forecasting stays off the request/hot path ───

func TestForecastHasNoHotPathDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	forbidden := map[string]bool{
		"github.com/talyvor/lens/internal/proxy": true,
		"github.com/talyvor/lens/internal/api":   true,
		"net/http":                               true,
	}
	for _, imp := range pkg.Imports { // non-test imports only
		if forbidden[imp] {
			t.Errorf("forecast must not import %q — it must stay read-only and off the request/hot path", imp)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
