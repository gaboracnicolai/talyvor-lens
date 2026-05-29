// Package forecast projects future AI spend from historical token_events,
// scoped to the same workspace / team / sprint dimensions as budgets
// (internal/budgets). It answers "at the current pace, what will this
// period cost, and will it blow the budget?"
//
// Design constraints (Master Plan Upgrade 20):
//
//   - READ-ONLY and OFF the request hot path. This package has no
//     dependency on the proxy / request layer (enforced structurally by a
//     test). Forecasts are computed from analytics reads and cached with a
//     short TTL so dashboard/API polls don't hammer Postgres.
//   - TRANSPARENT, no ML. Two explainable methods: a linear run-rate
//     (spend-so-far / elapsed-fraction → period total) and a trailing-window
//     daily rate (avg over the last N days), plus a steady/accelerating/
//     decelerating trend label. Every Forecast reports which method produced
//     it and the inputs behind it.
//   - HONEST. Forecasts are projections, never guarantees: field names and
//     the confidence note say so, and thin data yields an explicit
//     insufficient-data state rather than a wild extrapolation.
//
// Scope/period logic is reused from internal/budgets (ScopeColumn,
// PeriodBounds, ReconcileSpent) rather than duplicated here.
package forecast

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/metrics"
)

// Method names the calculation that produced a forecast's projected total.
type Method string

const (
	MethodLinearRunRate  Method = "linear_run_rate"
	MethodTrailingWindow Method = "trailing_window"
	MethodInsufficient   Method = "insufficient_data"
)

// TrendLabel is a plain-language direction, not a model output.
type TrendLabel string

const (
	TrendAccelerating TrendLabel = "accelerating"
	TrendSteady       TrendLabel = "steady"
	TrendDecelerating TrendLabel = "decelerating"
	TrendUnknown      TrendLabel = "unknown"
)

const (
	defaultTrailingDays = 7
	defaultTTL          = 5 * time.Minute

	// minDataDays is the floor of distinct in-period days with spend before
	// we'll project at all — below this we report insufficient data rather
	// than extrapolate from one or two points.
	minDataDays = 2
	// minElapsedFraction guards the "period just started" case: projecting a
	// period total by dividing by a tiny elapsed fraction explodes, so below
	// this we decline to project.
	minElapsedFraction = 0.05
	// trendBand is the ±fraction around 1.0 within which the recent vs prior
	// rate is called "steady".
	trendBand = 0.15
)

// DayBucket is one day's summed spend for a scope (from token_events).
type DayBucket struct {
	Day      time.Time `json:"day"`
	SpendUSD float64   `json:"spend_usd"`
}

// VsBudget compares a projection against the matching budget's limit. All
// fields are projections — never realized facts.
type VsBudget struct {
	LimitUSD             float64    `json:"limit_usd"`
	ProjectedOverageUSD  float64    `json:"projected_overage_usd"`
	ProjectedUtilization float64    `json:"projected_utilization"`
	WillExceed           bool       `json:"will_exceed"`
	EstExhaustionDate    *time.Time `json:"est_exhaustion_date,omitempty"`
}

// Forecast is a projection for one scope+period. Names use "projected" /
// "est" deliberately so consumers never mistake it for actual spend.
type Forecast struct {
	WorkspaceID       string        `json:"workspace_id"`
	Scope             budgets.Scope `json:"scope"`
	ScopeID           string        `json:"scope_id"`
	Period            string        `json:"period"`
	Method            Method        `json:"method"`
	ProjectedTotalUSD float64       `json:"projected_total_usd"`
	SpentSoFarUSD     float64       `json:"spent_so_far_usd"`
	ElapsedFraction   float64       `json:"elapsed_fraction"`
	DailyRateUSD      float64       `json:"daily_rate_usd"`
	TrendLabel        TrendLabel    `json:"trend_label"`
	DataDays          int           `json:"data_days"`
	ConfidenceNote    string        `json:"confidence_note"`
	InsufficientData  bool          `json:"insufficient_data"`
	VsBudget          *VsBudget     `json:"vs_budget,omitempty"`
	GeneratedAt       time.Time     `json:"generated_at"`
}

// source is the read surface the Forecaster needs. *Store implements it in
// production; tests pass a mock. Defining it here (rather than depending on
// a concrete store) keeps the package free of the request path and makes
// the cache + math testable without a database.
type source interface {
	// PeriodSpent returns realized spend for the scope over the period —
	// reused from budgets.Store.ReconcileSpent (single source of truth).
	PeriodSpent(ctx context.Context, b budgets.Budget) (float64, error)
	// DailyBuckets returns per-day spend for the scope over the last
	// sinceDays days (days with no spend simply absent).
	DailyBuckets(ctx context.Context, ws string, scope budgets.Scope, scopeID string, sinceDays int, now time.Time) ([]DayBucket, error)
	// Budgets lists the workspace's budgets (for limits + the summary view).
	Budgets(ctx context.Context, ws string) ([]budgets.Budget, error)
}

type cacheEntry struct {
	fc Forecast
	at time.Time
}

// Forecaster computes + caches projections. Safe for concurrent use.
type Forecaster struct {
	src          source
	trailingDays int
	ttl          time.Duration
	now          func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New builds a Forecaster over src (a *Store in production).
func New(src source) *Forecaster {
	return &Forecaster{
		src:          src,
		trailingDays: defaultTrailingDays,
		ttl:          defaultTTL,
		now:          time.Now,
		cache:        make(map[string]cacheEntry),
	}
}

// ─── pure, independently-testable math ───

// linearRunRate projects a period total: spend-so-far / elapsed-fraction.
// Half a period elapsed with $X spent → $2X projected.
func linearRunRate(spent, elapsedFraction float64) float64 {
	if elapsedFraction <= 0 {
		return 0
	}
	return spent / elapsedFraction
}

// trailingWindow projects from a flat daily rate over the last windowDays:
// dailyRate = windowSpend/windowDays; projected = spent + dailyRate*remainingDays.
func trailingWindow(windowSpend float64, windowDays int, spent, remainingDays float64) (projected, dailyRate float64) {
	if windowDays <= 0 {
		return spent, 0
	}
	dailyRate = windowSpend / float64(windowDays)
	if remainingDays < 0 {
		remainingDays = 0
	}
	return spent + dailyRate*remainingDays, dailyRate
}

// elapsedFraction is (now-start)/(end-start), clamped to [0,1].
func elapsedFraction(start, end, now time.Time) float64 {
	total := end.Sub(start)
	if total <= 0 {
		return 0
	}
	f := float64(now.Sub(start)) / float64(total)
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// trend compares the recent daily rate to the prior window's rate.
func trend(recentRate, priorRate float64) TrendLabel {
	if recentRate == 0 && priorRate == 0 {
		return TrendSteady
	}
	if priorRate <= 0 {
		// Was zero, now positive — spend just started ramping.
		return TrendAccelerating
	}
	ratio := recentRate / priorRate
	switch {
	case ratio >= 1+trendBand:
		return TrendAccelerating
	case ratio <= 1-trendBand:
		return TrendDecelerating
	default:
		return TrendSteady
	}
}

// confidenceNote phrases how much data backs the forecast. Few points →
// explicitly low confidence.
func confidenceNote(dataDays int) string {
	switch {
	case dataDays < 5:
		return fmt.Sprintf("based on %d day(s) of data — low confidence; treat as a rough projection", dataDays)
	case dataDays < 14:
		return fmt.Sprintf("based on %d days of data — moderate confidence", dataDays)
	default:
		return fmt.Sprintf("based on %d days of data — reasonable confidence", dataDays)
	}
}

func insufficientNote(dataDays int, hasBounds bool, elapsed float64) string {
	switch {
	case !hasBounds:
		return "open-ended period — no fixed end to project a period total toward"
	case elapsed < minElapsedFraction:
		return "period just started — too early to project; not extrapolating from a tiny sample"
	default:
		return fmt.Sprintf("insufficient data — only %d day(s) of spend so far; need at least %d to project", dataDays, minDataDays)
	}
}

// exhaustionDate estimates when cumulative spend hits the limit at the
// current daily rate. ok=false when it won't (zero rate) — already-exhausted
// returns now.
func exhaustionDate(limit, spent, dailyRate float64, now time.Time) (time.Time, bool) {
	if spent >= limit {
		return now, true
	}
	if dailyRate <= 0 {
		return time.Time{}, false
	}
	daysLeft := (limit - spent) / dailyRate
	return now.Add(time.Duration(daysLeft * float64(24*time.Hour))), true
}

// ─── bucket helpers ───

func sumRange(buckets []DayBucket, from, to time.Time) float64 {
	var s float64
	for _, b := range buckets {
		if !b.Day.Before(from) && b.Day.Before(to) {
			s += b.SpendUSD
		}
	}
	return s
}

func countDaysWithSpend(buckets []DayBucket, from, to time.Time) int {
	n := 0
	for _, b := range buckets {
		if b.SpendUSD > 0 && !b.Day.Before(from) && !b.Day.After(to) {
			n++
		}
	}
	return n
}

// ─── orchestration ───

// ProjectScope returns a cached-or-freshly-computed forecast for one scope.
// Workspace scope defaults scope_id to the workspace id; empty period
// defaults to monthly.
func (f *Forecaster) ProjectScope(ctx context.Context, ws string, scope budgets.Scope, scopeID, period string) (Forecast, error) {
	if scope == budgets.ScopeWorkspace && scopeID == "" {
		scopeID = ws
	}
	if period == "" {
		period = "monthly"
	}
	key := ws + "|" + string(scope) + "|" + scopeID + "|" + period
	now := f.now()

	f.mu.Lock()
	if e, ok := f.cache[key]; ok && now.Sub(e.at) < f.ttl {
		f.mu.Unlock()
		return e.fc, nil
	}
	f.mu.Unlock()

	fc, err := f.compute(ctx, ws, scope, scopeID, period, now)
	if err != nil {
		return fc, err
	}

	f.mu.Lock()
	f.cache[key] = cacheEntry{fc: fc, at: now}
	f.mu.Unlock()
	return fc, nil
}

func (f *Forecaster) compute(ctx context.Context, ws string, scope budgets.Scope, scopeID, period string, now time.Time) (Forecast, error) {
	fc := Forecast{
		WorkspaceID: ws, Scope: scope, ScopeID: scopeID, Period: period,
		GeneratedAt: now, TrendLabel: TrendUnknown,
		Method: MethodInsufficient, InsufficientData: true,
	}

	spent, err := f.src.PeriodSpent(ctx, budgets.Budget{WorkspaceID: ws, Scope: scope, ScopeID: scopeID, Period: period})
	if err != nil {
		return fc, err
	}
	fc.SpentSoFarUSD = spent

	start, end, hasBounds := budgets.PeriodBounds(period, now)

	// Fetch enough daily history to cover both the trend windows (2×
	// trailing) and the elapsed portion of the period.
	lookback := f.trailingDays * 2
	if hasBounds {
		if d := int(now.Sub(start).Hours()/24) + 1; d > lookback {
			lookback = d
		}
	}
	buckets, err := f.src.DailyBuckets(ctx, ws, scope, scopeID, lookback, now)
	if err != nil {
		return fc, err
	}

	// Trailing-window daily rate + trend (recent vs prior window).
	recent := sumRange(buckets, now.AddDate(0, 0, -f.trailingDays), now)
	prior := sumRange(buckets, now.AddDate(0, 0, -2*f.trailingDays), now.AddDate(0, 0, -f.trailingDays))
	dailyRate := recent / float64(f.trailingDays)
	priorRate := prior / float64(f.trailingDays)
	fc.DailyRateUSD = dailyRate
	fc.TrendLabel = trend(dailyRate, priorRate)

	dataStart := start
	if !hasBounds {
		dataStart = now.AddDate(0, 0, -lookback)
	}
	fc.DataDays = countDaysWithSpend(buckets, dataStart, now)

	var elapsed float64
	if hasBounds {
		elapsed = elapsedFraction(start, end, now)
	}
	fc.ElapsedFraction = elapsed

	sufficient := hasBounds && fc.DataDays >= minDataDays && elapsed >= minElapsedFraction
	if sufficient {
		fc.Method = MethodLinearRunRate
		fc.InsufficientData = false
		fc.ProjectedTotalUSD = linearRunRate(spent, elapsed)
		fc.ConfidenceNote = confidenceNote(fc.DataDays)
	} else {
		fc.Method = MethodInsufficient
		fc.InsufficientData = true
		fc.ProjectedTotalUSD = 0
		fc.ConfidenceNote = insufficientNote(fc.DataDays, hasBounds, elapsed)
	}

	if limit, ok := f.findLimit(ctx, ws, scope, scopeID); ok && limit > 0 {
		fc.VsBudget = buildVsBudget(limit, spent, fc.ProjectedTotalUSD, dailyRate, now, fc.InsufficientData)
	}

	// Metrics: bounded {scope} label only (no scope_id → cardinality safe).
	metrics.SetForecastProjected(string(scope), fc.ProjectedTotalUSD)
	if fc.VsBudget != nil && fc.VsBudget.WillExceed {
		metrics.ForecastWillExceed(string(scope))
	}
	return fc, nil
}

func buildVsBudget(limit, spent, projected, dailyRate float64, now time.Time, insufficient bool) *VsBudget {
	vb := &VsBudget{LimitUSD: limit}
	if insufficient {
		// No projection-based verdict when we declined to project; the
		// dashboard still shows spent-vs-limit from the budget itself.
		return vb
	}
	vb.ProjectedUtilization = projected / limit
	vb.WillExceed = projected > limit
	if vb.WillExceed {
		vb.ProjectedOverageUSD = projected - limit
	}
	if est, ok := exhaustionDate(limit, spent, dailyRate, now); ok {
		vb.EstExhaustionDate = &est
	}
	return vb
}

// findLimit looks up the limit for the matching budget, reusing the budgets
// list rather than re-querying scope logic.
func (f *Forecaster) findLimit(ctx context.Context, ws string, scope budgets.Scope, scopeID string) (float64, bool) {
	bs, err := f.src.Budgets(ctx, ws)
	if err != nil {
		return 0, false
	}
	for _, b := range bs {
		if b.Scope == scope && b.ScopeID == scopeID {
			return b.LimitUSD, true
		}
	}
	return 0, false
}

// SummarizeWorkspace projects every budget in the workspace — the view the
// dashboard renders so operators see everything trending over budget.
func (f *Forecaster) SummarizeWorkspace(ctx context.Context, ws string) ([]Forecast, error) {
	bs, err := f.src.Budgets(ctx, ws)
	if err != nil {
		return nil, err
	}
	out := make([]Forecast, 0, len(bs))
	for _, b := range bs {
		fc, err := f.ProjectScope(ctx, ws, b.Scope, b.ScopeID, b.Period)
		if err != nil {
			continue // best-effort per budget
		}
		out = append(out, fc)
	}
	return out, nil
}
