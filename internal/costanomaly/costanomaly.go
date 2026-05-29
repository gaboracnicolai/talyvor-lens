// Package costanomaly detects CROSS-SECTIONAL cost outliers: a unit of
// work (an issue, or a team/sprint) whose total AI cost is abnormally high
// versus the distribution of comparable units over a trailing window —
// e.g. "ENG-88 cost 4× the median issue this window."
//
// This is deliberately distinct from internal/anomaly, which is a TEMPORAL
// detector (is this hour's spend a z-score spike vs this dimension's own
// recent history). costanomaly compares one unit against its PEERS at a
// point in time, using robust statistics (median + MAD).
//
// Honest by construction:
//   - Transparent statistics, NO ML. Every anomaly reports its method, the
//     actual numbers ($ this vs median vs threshold), and the sample size.
//   - A minimum baseline (≥ MinSample comparable units) is required before
//     anything is flagged — with a handful of units, "4× the median" is
//     noise, not signal. Below it → InsufficientBaseline, no anomaly.
//   - "Comparable" means same-scope-over-the-window (issues in the same
//     workspace; teams/sprints across the workspace), NOT semantic
//     similarity. Anomalies are statistical FLAGS, not judgments.
//
// Read-only analytics, cached with a TTL, and OFF the request hot path
// (enforced by an import-parsing structural test). Reuses budgets scope
// helpers; never touches the proxy/request layer.
package costanomaly

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// Method names the statistic used to set the anomaly threshold.
type Method string

const (
	// MethodMAD flags cost > median + N×MAD (median absolute deviation —
	// robust to skew, the default).
	MethodMAD Method = "mad"
	// MethodMultipleMedian flags cost > K×median.
	MethodMultipleMedian Method = "multiple_median"
)

// Severity is derived from the multiple-of-median factor (a bounded set).
type Severity string

const (
	SeverityLow  Severity = "low"  // flagged, but < 3× median
	SeverityWarn Severity = "warn" // 3×–5× median
	SeverityHigh Severity = "high" // > 5× median
)

// Unit kinds (the "scope" of a scan — the dimension whose peers are compared).
const (
	UnitIssue  = "issue"
	UnitTeam   = "team"
	UnitSprint = "sprint"
)

const materialWorse = 1.25 // re-report a deduped anomaly only if its factor grows ≥25%.

// Config holds the (transparent, configurable) detection parameters.
type Config struct {
	Method     Method        // default MethodMAD
	K          float64       // multiple-of-median factor (default 3)
	N          float64       // MAD multiplier (default 3)
	MinSample  int           // minimum comparable units (default 5)
	WindowDays int           // trailing window in days (default 30)
	TTL        time.Duration // cache TTL (default 5m)
}

// DefaultConfig is the robust default: MAD method, k=n=3, ≥5 samples,
// 30-day window, 5-minute cache.
func DefaultConfig() Config {
	return Config{Method: MethodMAD, K: 3, N: 3, MinSample: 5, WindowDays: 30, TTL: 5 * time.Minute}
}

// UnitCost is one unit's summed cost over the window.
type UnitCost struct {
	UnitID  string  `json:"unit_id"`
	CostUSD float64 `json:"cost_usd"`
}

// Anomaly is one flagged unit. All fields are facts about the distribution,
// not a judgment of correctness.
type Anomaly struct {
	WorkspaceID    string    `json:"workspace_id"`
	Scope          string    `json:"scope"`
	UnitID         string    `json:"unit_id"`
	CostUSD        float64   `json:"cost_usd"`
	BaselineMedian float64   `json:"baseline_median"`
	ThresholdUSD   float64   `json:"threshold_usd"`
	Factor         float64   `json:"factor"`
	Method         Method    `json:"method"`
	SampleSize     int       `json:"sample_size"`
	Severity       Severity  `json:"severity"`
	DetectedAt     time.Time `json:"detected_at"`
	Explanation    string    `json:"explanation"`
}

// ScanResult is the outcome of scanning one scope. InsufficientBaseline is
// true (with empty Anomalies) when there were fewer than MinSample units.
type ScanResult struct {
	WorkspaceID          string    `json:"workspace_id"`
	Scope                string    `json:"scope"`
	Method               Method    `json:"method"`
	SampleSize           int       `json:"sample_size"`
	BaselineMedian       float64   `json:"baseline_median"`
	ThresholdUSD         float64   `json:"threshold_usd"`
	InsufficientBaseline bool      `json:"insufficient_baseline"`
	Anomalies            []Anomaly `json:"anomalies"`
	GeneratedAt          time.Time `json:"generated_at"`
}

// IssueAssessment answers "is this specific issue anomalous?" — always with
// the numbers, whether or not it crossed the threshold.
type IssueAssessment struct {
	WorkspaceID          string    `json:"workspace_id"`
	IssueID              string    `json:"issue_id"`
	CostUSD              float64   `json:"cost_usd"`
	BaselineMedian       float64   `json:"baseline_median"`
	ThresholdUSD         float64   `json:"threshold_usd"`
	Factor               float64   `json:"factor"`
	Method               Method    `json:"method"`
	SampleSize           int       `json:"sample_size"`
	Anomalous            bool      `json:"anomalous"`
	InsufficientBaseline bool      `json:"insufficient_baseline"`
	Severity             Severity  `json:"severity,omitempty"`
	Explanation          string    `json:"explanation"`
	GeneratedAt          time.Time `json:"generated_at"`
}

// unitSource is the read surface the Detector needs — implemented by *Store
// in production, a mock in tests. Defining it here keeps the package off
// the request path and the statistics testable without a database.
type unitSource interface {
	// UnitCosts returns the summed cost per unit for the given unit kind
	// (issue → request_attribution; team/sprint → token_events) since the
	// given time. Only units with positive spend are returned.
	UnitCosts(ctx context.Context, workspaceID, unitKind string, since time.Time) ([]UnitCost, error)
}

type costsEntry struct {
	costs []UnitCost
	at    time.Time
}

type seenEntry struct{ factor float64 }

// Detector computes + caches cross-sectional cost anomalies. Safe for
// concurrent use.
type Detector struct {
	src unitSource
	cfg Config
	now func() time.Time

	mu         sync.Mutex
	costsCache map[string]costsEntry // ws|scope → costs (TTL'd)
	seen       map[string]seenEntry  // ws|scope|unit|period → last reported factor (dedupe)
}

// New builds a Detector with DefaultConfig over src (a *Store in production).
func New(src unitSource) *Detector { return NewWithConfig(src, DefaultConfig()) }

// NewWithConfig builds a Detector with an explicit config; zero fields fall
// back to the defaults so partial configs are safe.
func NewWithConfig(src unitSource, cfg Config) *Detector {
	def := DefaultConfig()
	if cfg.Method == "" {
		cfg.Method = def.Method
	}
	if cfg.K <= 0 {
		cfg.K = def.K
	}
	if cfg.N <= 0 {
		cfg.N = def.N
	}
	if cfg.MinSample <= 0 {
		cfg.MinSample = def.MinSample
	}
	if cfg.WindowDays <= 0 {
		cfg.WindowDays = def.WindowDays
	}
	if cfg.TTL <= 0 {
		cfg.TTL = def.TTL
	}
	return &Detector{
		src:        src,
		cfg:        cfg,
		now:        time.Now,
		costsCache: make(map[string]costsEntry),
		seen:       make(map[string]seenEntry),
	}
}

// ─── pure statistics (independently testable) ───

func medianOf(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	s := make([]float64, n)
	copy(s, values)
	sort.Float64s(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// madOf is the median absolute deviation: median(|x - med|).
func madOf(values []float64, med float64) float64 {
	if len(values) == 0 {
		return 0
	}
	dev := make([]float64, len(values))
	for i, v := range values {
		d := v - med
		if d < 0 {
			d = -d
		}
		dev[i] = d
	}
	return medianOf(dev)
}

// thresholdFor returns the baseline median and the cost threshold above
// which a unit is anomalous. For the MAD method with zero spread (a flat
// baseline) it falls back to K×median so we don't flag a unit a cent above
// an identical peer set.
func (cfg Config) thresholdFor(values []float64) (median, threshold float64) {
	median = medianOf(values)
	if cfg.Method == MethodMultipleMedian {
		return median, cfg.K * median
	}
	m := madOf(values, median)
	if m <= 0 {
		return median, cfg.K * median
	}
	return median, median + cfg.N*m
}

// severityFor maps the multiple-of-median factor to a tier. Boundaries:
// factor > 5 → high; 3 ≤ factor ≤ 5 → warn; otherwise low.
func severityFor(factor float64) Severity {
	switch {
	case factor > 5:
		return SeverityHigh
	case factor >= 3:
		return SeverityWarn
	default:
		return SeverityLow
	}
}

func explain(scope, unitID string, cost, median, threshold float64, factor float64, method Method, sample int) string {
	return fmt.Sprintf(
		"%s %q cost $%.2f — %.1f× the baseline median $%.2f across %d comparable %ss in this workspace (method %s, threshold $%.2f). Statistical flag, not a judgment.",
		scope, unitID, cost, factor, median, sample, scope, method, threshold,
	)
}

// ─── orchestration ───

// cachedCosts returns the unit costs for a scope, served from the TTL cache
// when fresh. The bool reports whether this was a fresh fetch (cache miss),
// so callers fire metrics/dedupe only on fresh data.
func (d *Detector) cachedCosts(ctx context.Context, ws, scope string, now time.Time) ([]UnitCost, bool, error) {
	key := ws + "|" + scope
	d.mu.Lock()
	if e, ok := d.costsCache[key]; ok && now.Sub(e.at) < d.cfg.TTL {
		c := e.costs
		d.mu.Unlock()
		return c, false, nil
	}
	d.mu.Unlock()

	since := now.AddDate(0, 0, -d.cfg.WindowDays)
	costs, err := d.src.UnitCosts(ctx, ws, scope, since)
	if err != nil {
		return nil, false, err
	}
	d.mu.Lock()
	d.costsCache[key] = costsEntry{costs: costs, at: now}
	d.mu.Unlock()
	return costs, true, nil
}

// ScanScope returns every anomalous unit in the scope (issue/team/sprint).
// Read-only + cached. Metrics + dedupe fire only on a fresh fetch.
func (d *Detector) ScanScope(ctx context.Context, ws, scope string) (ScanResult, error) {
	now := d.now()
	costs, fresh, err := d.cachedCosts(ctx, ws, scope, now)
	if err != nil {
		return ScanResult{}, err
	}

	res := ScanResult{
		WorkspaceID: ws, Scope: scope, Method: d.cfg.Method,
		SampleSize: len(costs), GeneratedAt: now,
	}
	if len(costs) < d.cfg.MinSample {
		res.InsufficientBaseline = true
		return res, nil
	}

	values := make([]float64, len(costs))
	for i, c := range costs {
		values[i] = c.CostUSD
	}
	median, threshold := d.cfg.thresholdFor(values)
	res.BaselineMedian = median
	res.ThresholdUSD = threshold

	periodKey := now.Format("2006-01")
	var maxFactor float64
	for _, uc := range costs {
		if uc.CostUSD <= threshold {
			continue
		}
		factor := 0.0
		if median > 0 {
			factor = uc.CostUSD / median
		}
		sev := severityFor(factor)
		res.Anomalies = append(res.Anomalies, Anomaly{
			WorkspaceID: ws, Scope: scope, UnitID: uc.UnitID,
			CostUSD: uc.CostUSD, BaselineMedian: median, ThresholdUSD: threshold,
			Factor: factor, Method: d.cfg.Method, SampleSize: len(costs),
			Severity: sev, DetectedAt: now,
			Explanation: explain(scope, uc.UnitID, uc.CostUSD, median, threshold, factor, d.cfg.Method, len(costs)),
		})
		if factor > maxFactor {
			maxFactor = factor
		}
		// Fire metrics once per unit+period (deduped) and only on a fresh
		// fetch, so repeated polls within the TTL don't re-count.
		if fresh && d.shouldReport(ws, scope, uc.UnitID, periodKey, factor) {
			metrics.AnomalyDetected(scope, string(sev))
		}
	}
	if fresh && maxFactor > 0 {
		metrics.SetAnomalyMaxFactor(scope, maxFactor)
	}
	return res, nil
}

// CheckIssue reports a single issue's standing against the workspace issue
// baseline — always with the numbers, anomalous or not.
func (d *Detector) CheckIssue(ctx context.Context, ws, issueID string) (IssueAssessment, error) {
	now := d.now()
	costs, _, err := d.cachedCosts(ctx, ws, UnitIssue, now)
	if err != nil {
		return IssueAssessment{}, err
	}
	a := IssueAssessment{
		WorkspaceID: ws, IssueID: issueID, Method: d.cfg.Method,
		SampleSize: len(costs), GeneratedAt: now,
	}
	for _, c := range costs {
		if c.UnitID == issueID {
			a.CostUSD = c.CostUSD
		}
	}
	if len(costs) < d.cfg.MinSample {
		a.InsufficientBaseline = true
		a.Explanation = fmt.Sprintf("insufficient baseline — only %d comparable issue(s) with spend in this workspace over the last %d days; need at least %d to judge an outlier.", len(costs), d.cfg.WindowDays, d.cfg.MinSample)
		return a, nil
	}
	values := make([]float64, len(costs))
	for i, c := range costs {
		values[i] = c.CostUSD
	}
	median, threshold := d.cfg.thresholdFor(values)
	a.BaselineMedian = median
	a.ThresholdUSD = threshold
	if median > 0 {
		a.Factor = a.CostUSD / median
	}
	a.Anomalous = a.CostUSD > threshold
	if a.Anomalous {
		a.Severity = severityFor(a.Factor)
	}
	a.Explanation = explain(UnitIssue, issueID, a.CostUSD, median, threshold, a.Factor, d.cfg.Method, len(costs))
	return a, nil
}

// shouldReport gates anomaly side-effects so the same anomaly isn't
// re-reported within a period unless it materially worsens. Caller holds no
// lock; this takes d.mu.
func (d *Detector) shouldReport(ws, scope, unitID, periodKey string, factor float64) bool {
	key := ws + "|" + scope + "|" + unitID + "|" + periodKey
	d.mu.Lock()
	defer d.mu.Unlock()
	prev, ok := d.seen[key]
	if !ok || factor >= prev.factor*materialWorse {
		d.seen[key] = seenEntry{factor: factor}
		return true
	}
	return false
}
