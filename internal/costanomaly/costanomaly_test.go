package costanomaly

import (
	"context"
	"go/build"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/talyvor/lens/internal/metrics"
)

type mockSource struct {
	costs map[string][]UnitCost
	calls int
}

func (m *mockSource) UnitCosts(_ context.Context, _ string, unitKind string, _ time.Time) ([]UnitCost, error) {
	m.calls++
	return m.costs[unitKind], nil
}

func units(costs ...float64) []UnitCost {
	out := make([]UnitCost, len(costs))
	for i, c := range costs {
		out[i] = UnitCost{UnitID: "U" + string(rune('A'+i)), CostUSD: c}
	}
	return out
}

func newDetectorAt(src unitSource, now time.Time) *Detector {
	d := New(src)
	d.now = func() time.Time { return now }
	return d
}

var fixedNow = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

// ─── MAD detection ───

func TestMAD_FlagsOutlierIgnoresNormalVariance(t *testing.T) {
	ctx := context.Background()

	// Normal variance: tight cluster, nothing should flag.
	normal := &mockSource{costs: map[string][]UnitCost{
		UnitIssue: units(10, 11, 12, 9, 10, 11, 13),
	}}
	res, err := newDetectorAt(normal, fixedNow).ScanScope(ctx, "ws1", UnitIssue)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Anomalies) != 0 {
		t.Fatalf("normal variance must not flag: %+v", res.Anomalies)
	}

	// Clear outlier: $50 against a ~$11 median.
	outlier := &mockSource{costs: map[string][]UnitCost{
		UnitIssue: units(10, 11, 12, 9, 10, 11, 50),
	}}
	res, err = newDetectorAt(outlier, fixedNow).ScanScope(ctx, "ws1", UnitIssue)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Anomalies) != 1 {
		t.Fatalf("expected exactly 1 anomaly, got %d: %+v", len(res.Anomalies), res.Anomalies)
	}
	a := res.Anomalies[0]
	if a.CostUSD != 50 {
		t.Fatalf("anomaly cost: got %.2f want 50", a.CostUSD)
	}
	if a.BaselineMedian != 11 {
		t.Fatalf("baseline median: got %.2f want 11", a.BaselineMedian)
	}
	if a.Severity != SeverityWarn { // 50/11 ≈ 4.5×
		t.Fatalf("severity: got %q want warn", a.Severity)
	}
}

// ─── multiple-of-median ───

func TestMultipleMedian_Correct(t *testing.T) {
	ctx := context.Background()
	src := &mockSource{costs: map[string][]UnitCost{
		UnitTeam: units(10, 10, 10, 10, 10, 30, 40),
	}}
	d := NewWithConfig(src, Config{Method: MethodMultipleMedian, K: 3})
	d.now = func() time.Time { return fixedNow }
	res, err := d.ScanScope(ctx, "ws1", UnitTeam)
	if err != nil {
		t.Fatal(err)
	}
	// median = 10, threshold = 3×10 = 30. Only 40 (>30) flags; 30 is NOT
	// flagged (strictly greater).
	if len(res.Anomalies) != 1 || res.Anomalies[0].CostUSD != 40 {
		t.Fatalf("expected only the $40 unit flagged: %+v", res.Anomalies)
	}
	if res.ThresholdUSD != 30 {
		t.Fatalf("threshold: got %.2f want 30", res.ThresholdUSD)
	}
	if res.Method != MethodMultipleMedian {
		t.Fatalf("method: got %q", res.Method)
	}
}

// ─── insufficient baseline ───

func TestInsufficientBaseline_NoAnomaly(t *testing.T) {
	ctx := context.Background()
	// Only 4 units (< default MinSample of 5), one obviously huge.
	src := &mockSource{costs: map[string][]UnitCost{
		UnitIssue: units(10, 11, 12, 500),
	}}
	res, err := newDetectorAt(src, fixedNow).ScanScope(ctx, "ws1", UnitIssue)
	if err != nil {
		t.Fatal(err)
	}
	if !res.InsufficientBaseline {
		t.Fatalf("4 units must be insufficient baseline")
	}
	if len(res.Anomalies) != 0 {
		t.Fatalf("insufficient baseline must yield no anomalies, got %+v", res.Anomalies)
	}
}

func TestCheckIssue_InsufficientBaselineExplained(t *testing.T) {
	ctx := context.Background()
	src := &mockSource{costs: map[string][]UnitCost{
		UnitIssue: {{UnitID: "ENG-88", CostUSD: 99}, {UnitID: "ENG-1", CostUSD: 1}},
	}}
	a, err := newDetectorAt(src, fixedNow).CheckIssue(ctx, "ws1", "ENG-88")
	if err != nil {
		t.Fatal(err)
	}
	if !a.InsufficientBaseline || a.Anomalous {
		t.Fatalf("tiny baseline must be insufficient + not anomalous: %+v", a)
	}
	if a.CostUSD != 99 {
		t.Fatalf("should still report the issue's own cost: got %.2f", a.CostUSD)
	}
	if !strings.Contains(a.Explanation, "insufficient baseline") {
		t.Fatalf("explanation should say insufficient: %q", a.Explanation)
	}
}

// ─── severity boundaries ───

func TestSeverityBoundaries(t *testing.T) {
	cases := []struct {
		factor float64
		want   Severity
	}{
		{2.99, SeverityLow},
		{3.0, SeverityWarn},
		{5.0, SeverityWarn},
		{5.0001, SeverityHigh},
		{10, SeverityHigh},
	}
	for _, c := range cases {
		if got := severityFor(c.factor); got != c.want {
			t.Errorf("severityFor(%.4f): got %q want %q", c.factor, got, c.want)
		}
	}
}

// ─── dedupe ───

func TestDedupe_ReportsOncePerPeriodUntilWorse(t *testing.T) {
	d := New(&mockSource{})
	const ws, scope, unit, period = "ws1", UnitIssue, "ENG-88", "2026-05"

	if !d.shouldReport(ws, scope, unit, period, 4.0) {
		t.Fatal("first detection should report")
	}
	if d.shouldReport(ws, scope, unit, period, 4.0) {
		t.Fatal("same factor in same period must NOT re-report")
	}
	if d.shouldReport(ws, scope, unit, period, 4.5) {
		t.Fatal("a 4.5 vs 4.0 (<25% worse) must NOT re-report")
	}
	if !d.shouldReport(ws, scope, unit, period, 5.0) {
		t.Fatal("materially worse (≥25%) must re-report")
	}
	// A new period re-reports even at the same factor.
	if !d.shouldReport(ws, scope, unit, "2026-06", 5.0) {
		t.Fatal("a new period should report again")
	}
}

// ─── explanation carries the actual numbers ───

func TestExplanation_ContainsActualNumbers(t *testing.T) {
	ctx := context.Background()
	src := &mockSource{costs: map[string][]UnitCost{
		UnitIssue: units(10, 11, 12, 9, 10, 11, 50),
	}}
	res, _ := newDetectorAt(src, fixedNow).ScanScope(ctx, "ws1", UnitIssue)
	if len(res.Anomalies) != 1 {
		t.Fatalf("setup: expected 1 anomaly, got %d", len(res.Anomalies))
	}
	e := res.Anomalies[0].Explanation
	for _, want := range []string{"$50.00", "$11.00", "Statistical flag, not a judgment"} {
		if !strings.Contains(e, want) {
			t.Errorf("explanation missing %q: %s", want, e)
		}
	}
}

// ─── cache ───

func TestScanScope_CachesWithinTTL(t *testing.T) {
	ctx := context.Background()
	src := &mockSource{costs: map[string][]UnitCost{UnitIssue: units(10, 11, 12, 9, 10, 11, 50)}}
	clock := fixedNow
	d := New(src)
	d.now = func() time.Time { return clock }

	_, _ = d.ScanScope(ctx, "ws1", UnitIssue)
	_, _ = d.ScanScope(ctx, "ws1", UnitIssue)
	if src.calls != 1 {
		t.Fatalf("within TTL the store must be queried once: got %d", src.calls)
	}
	clock = fixedNow.Add(d.cfg.TTL + time.Second)
	_, _ = d.ScanScope(ctx, "ws1", UnitIssue)
	if src.calls != 2 {
		t.Fatalf("after TTL expiry the store must be re-queried: got %d", src.calls)
	}
}

// ─── cardinality: bounded metric labels only ───

func TestMetrics_NoUnboundedCardinality(t *testing.T) {
	ctx := context.Background()
	// 50 distinct anomalous unit ids, all in (issue, *) — must NOT create
	// 50 metric series. The label set is {scope, severity} only.
	costs := make([]UnitCost, 0, 60)
	for i := 0; i < 5; i++ {
		costs = append(costs, UnitCost{UnitID: "base", CostUSD: 10})
	}
	for i := 0; i < 50; i++ {
		costs = append(costs, UnitCost{UnitID: "X" + string(rune(i)), CostUSD: 100})
	}
	src := &mockSource{costs: map[string][]UnitCost{UnitIssue: costs}}
	if _, err := newDetectorAt(src, fixedNow).ScanScope(ctx, "ws1", UnitIssue); err != nil {
		t.Fatal(err)
	}
	// 3 scopes × 3 severities = 9 max series, regardless of unit count.
	if n := testutil.CollectAndCount(metrics.AnomaliesDetectedTotal); n > 9 {
		t.Fatalf("anomalies_detected_total has %d series — unbounded labels leaked", n)
	}
}

// ─── structural: off the request/hot path ───

func TestCostAnomalyHasNoHotPathDependency(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	forbidden := map[string]bool{
		"github.com/talyvor/lens/internal/proxy": true,
		"github.com/talyvor/lens/internal/api":   true,
		"net/http":                               true,
	}
	for _, imp := range pkg.Imports {
		if forbidden[imp] {
			t.Errorf("costanomaly must not import %q — it must stay read-only and off the request/hot path", imp)
		}
	}
}
