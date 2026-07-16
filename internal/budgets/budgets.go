// Package budgets implements per-team / per-sprint budget governance
// (Master Plan Upgrade 19, Lens portion). It extends the existing
// per-workspace spend tracking into budgets scoped to workspace, team,
// or sprint.
//
// The hot path is split in two so it never touches Postgres per request:
//
//   - CheckBudget(...)  — reads an in-memory snapshot of active budgets
//     and their running spend, returns allow / alert / block using
//     most-restrictive-wins across the matching workspace+team+sprint
//     budgets.
//   - RecordSpend(...)  — increments the in-memory running totals after a
//     request bills, firing each alert threshold at most once per period.
//
// The in-memory snapshot is seeded + periodically reconciled from
// token_events (the single billing source of truth — see Store) via
// Load / Reload. spent_usd in the budgets table is a convenience
// snapshot written during reconciliation, never on the request path.
package budgets

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/metrics"
)

// Scope is the dimension a budget governs.
type Scope string

const (
	ScopeWorkspace Scope = "workspace"
	ScopeTeam      Scope = "team"
	ScopeSprint    Scope = "sprint"
)

// Enforcement controls what CheckBudget does when spend nears/exceeds the
// limit.
//
//	off        — track spend only; never alert, never block.
//	alert      — track + fire threshold alerts; never block (the default).
//	hard_block — track + alert + block requests once over the limit.
type Enforcement string

const (
	EnforcementOff       Enforcement = "off"
	EnforcementAlert     Enforcement = "alert"
	EnforcementHardBlock Enforcement = "hard_block"
)

// Decision is the verdict CheckBudget returns for a request.
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAlert Decision = "alert"
	DecisionBlock Decision = "block"
)

// rank orders decisions for most-restrictive-wins (higher = more restrictive).
func rank(d Decision) int {
	switch d {
	case DecisionBlock:
		return 2
	case DecisionAlert:
		return 1
	default:
		return 0
	}
}

// defaultMaxScopeSeries caps how many distinct scope_id label values the
// per-scope_id gauges may emit, so an unbounded team/sprint id space can't
// blow up Prometheus cardinality. Counters use only the bounded {scope}
// label and are never guarded.
const defaultMaxScopeSeries = 1000

// defaultRefreshInterval is how often Reload re-reconciles spend from
// token_events when StartRefresh is running.
const defaultRefreshInterval = 60 * time.Second

// Budget is one governance rule. ScopeID is the concrete identifier within
// the scope: the workspace id for ScopeWorkspace, the team id for
// ScopeTeam, the sprint id for ScopeSprint.
type Budget struct {
	ID              string      `json:"id"`
	WorkspaceID     string      `json:"workspace_id"`
	Scope           Scope       `json:"scope"`
	ScopeID         string      `json:"scope_id"`
	Period          string      `json:"period"`
	LimitUSD        float64     `json:"limit_usd"`
	SpentUSD        float64     `json:"spent_usd"`
	AlertThresholds []float64   `json:"alert_thresholds"`
	Enforcement     Enforcement `json:"enforcement"`
	StartsAt        *time.Time  `json:"starts_at,omitempty"`
	EndsAt          *time.Time  `json:"ends_at,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

// decide is the per-budget verdict for a projected spend total. Most-
// restrictive-wins across budgets is applied by the caller.
//
// A budget never blocks below its limit; the alert-threshold ladder is a
// notification concern handled in RecordSpend, not a gate. At exactly the
// limit (projected == limit) the request still passes — only strictly over
// blocks.
func (b Budget) decide(projected float64) Decision {
	if b.Enforcement == EnforcementOff || b.LimitUSD <= 0 {
		return DecisionAllow
	}
	if projected > b.LimitUSD {
		if b.Enforcement == EnforcementHardBlock {
			return DecisionBlock
		}
		// alert mode acknowledges the overage but never rejects.
		return DecisionAlert
	}
	return DecisionAllow
}

// Status is the API/dashboard view of a budget's live state.
type Status struct {
	Budget
	UtilizationRatio float64  `json:"utilization_ratio"`
	Decision         Decision `json:"decision"`
}

// BudgetAlert is handed to the alert sink when a threshold is first crossed
// within a period.
type BudgetAlert struct {
	Budget    Budget
	SpentUSD  float64
	Threshold float64
	Ratio     float64
}

// budgetState is the in-memory running view of one budget.
type budgetState struct {
	b       Budget
	spent   float64
	crossed map[float64]bool // alert thresholds already fired this period
}

// cardinalityGuard bounds the number of distinct keys that may be emitted
// as metric label values. Once max distinct keys have been seen, new keys
// are refused (and counted) so emission stays bounded.
type cardinalityGuard struct {
	mu      sync.Mutex
	max     int
	seen    map[string]struct{}
	dropped int
}

func newCardinalityGuard(max int) *cardinalityGuard {
	return &cardinalityGuard{max: max, seen: make(map[string]struct{})}
}

func (g *cardinalityGuard) allow(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.seen[key]; ok {
		return true
	}
	if len(g.seen) >= g.max {
		g.dropped++
		return false
	}
	g.seen[key] = struct{}{}
	return true
}

// Service holds the in-memory budget snapshot and the read/record hot path.
type Service struct {
	store *Store

	mu     sync.RWMutex
	states map[string]*budgetState

	guard *cardinalityGuard

	// alert is the notification sink for a freshly-crossed threshold.
	// Defaults to a structured slog line (no email — Lens has none). Tests
	// inject a capture func.
	alert func(BudgetAlert)

	refreshInterval time.Duration
}

// NewService builds a Service backed by store (which may be nil for pure
// in-memory use in tests).
func NewService(store *Store) *Service {
	return &Service{
		store:           store,
		states:          make(map[string]*budgetState),
		guard:           newCardinalityGuard(defaultMaxScopeSeries),
		alert:           defaultAlert,
		refreshInterval: defaultRefreshInterval,
	}
}

func defaultAlert(a BudgetAlert) {
	slog.Warn("budget: alert threshold crossed",
		slog.String("workspace_id", a.Budget.WorkspaceID),
		slog.String("scope", string(a.Budget.Scope)),
		slog.String("scope_id", a.Budget.ScopeID),
		slog.Float64("spent_usd", a.SpentUSD),
		slog.Float64("limit_usd", a.Budget.LimitUSD),
		slog.Float64("threshold", a.Threshold),
		slog.Float64("ratio", a.Ratio),
	)
}

// stateKey is the in-memory + scope-resolution key for a budget.
func stateKey(workspace string, scope Scope, scopeID string) string {
	return workspace + "|" + string(scope) + "|" + scopeID
}

// PeriodBounds returns the [start, end) calendar window for a budget period
// relative to now, in UTC. ok is false for the open-ended "total" period
// (callers handle that case themselves — there is no period total to
// project toward).
//
// The calendar definitions mirror store.go's periodWindow exactly so
// forecasting's elapsed-fraction math lines up with reconciliation's spend
// window: monthly = first of the month, weekly = Monday (matching Postgres
// date_trunc('week')). Bounds are computed in UTC; at a month/week boundary
// this can differ from a non-UTC database NOW() by the offset, which is
// immaterial for an elapsed-fraction estimate.
func PeriodBounds(period string, now time.Time) (start, end time.Time, ok bool) {
	now = now.UTC()
	switch period {
	case "weekly":
		// ISO week starts Monday. Go's Weekday has Sunday=0; shift so Mon=0.
		offset := (int(now.Weekday()) + 6) % 7
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -offset)
		return start, start.AddDate(0, 0, 7), true
	case "total":
		return time.Time{}, time.Time{}, false
	default: // monthly
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), true
	}
}

// candidates returns the keys of the up-to-three budgets that govern a
// request. The workspace budget is always a candidate; team / sprint are
// included only when their id is present.
func candidates(workspace, team, sprint string) []string {
	keys := []string{stateKey(workspace, ScopeWorkspace, workspace)}
	if team != "" {
		keys = append(keys, stateKey(workspace, ScopeTeam, team))
	}
	if sprint != "" {
		keys = append(keys, stateKey(workspace, ScopeSprint, sprint))
	}
	return keys
}

// loadStates rebuilds the in-memory snapshot from a fresh set of budgets.
// Each budget's already-passed thresholds are pre-marked as crossed from
// its reconciled spend, so a reload (or a restart) never re-fires an alert
// for a threshold the period already passed.
func (s *Service) loadStates(bs []Budget) {
	states := make(map[string]*budgetState, len(bs))
	for _, b := range bs {
		st := &budgetState{b: b, spent: b.SpentUSD, crossed: map[float64]bool{}}
		if b.LimitUSD > 0 {
			ratio := b.SpentUSD / b.LimitUSD
			for _, t := range b.AlertThresholds {
				if ratio >= t {
					st.crossed[t] = true
				}
			}
		}
		states[stateKey(b.WorkspaceID, b.Scope, b.ScopeID)] = st
	}
	s.mu.Lock()
	s.states = states
	s.mu.Unlock()
}

// Load seeds the in-memory snapshot from the store, reconciling each
// budget's spend from token_events.
func (s *Service) Load(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	bs, err := s.store.ActiveBudgets(ctx)
	if err != nil {
		return err
	}
	for i := range bs {
		spent, err := s.store.ReconcileSpent(ctx, bs[i])
		if err != nil {
			// Best-effort: fall back to the persisted snapshot rather than
			// failing the whole load on one bad reconcile.
			continue
		}
		bs[i].SpentUSD = spent
	}
	s.loadStates(bs)
	// Persist the reconciled snapshots so the API/dashboard read fresh
	// numbers without recomputing. Best-effort.
	for _, b := range bs {
		_ = s.store.UpdateSpent(ctx, b.WorkspaceID, b.ID, b.SpentUSD)
	}
	return nil
}

// Reload re-reconciles the snapshot. Called after a CRUD change (so budget
// edits take effect immediately) and on the periodic refresh tick.
func (s *Service) Reload(ctx context.Context) error { return s.Load(ctx) }

// StartRefresh runs Reload on a ticker until ctx is cancelled.
func (s *Service) StartRefresh(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	go func() {
		t := time.NewTicker(s.refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.Reload(ctx); err != nil {
					slog.Warn("budget: refresh reload failed", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// CheckBudget returns the most-restrictive verdict across the workspace,
// team, and sprint budgets that govern the request. estCost is the
// estimated USD cost of the pending request. Reads in-memory state only —
// no DB.
func (s *Service) CheckBudget(_ context.Context, workspace, team, sprint string, estCost float64) Decision {
	if s == nil {
		return DecisionAllow
	}
	s.mu.RLock()
	worst := DecisionAllow
	worstScope := string(ScopeWorkspace)
	for _, k := range candidates(workspace, team, sprint) {
		st := s.states[k]
		if st == nil {
			continue
		}
		if d := st.b.decide(st.spent + estCost); rank(d) > rank(worst) {
			worst = d
			worstScope = string(st.b.Scope)
		}
	}
	s.mu.RUnlock()

	if worst == DecisionBlock {
		metrics.BudgetBlocked(worstScope)
	}
	return worst
}

// RecordSpend adds a billed request's cost to every budget that governs it
// and fires any newly-crossed alert thresholds (once per threshold per
// period). In-memory only — token_events is the durable record, written on
// the same single billing write the cost came from.
func (s *Service) RecordSpend(_ context.Context, workspace, team, sprint string, cost float64) {
	if s == nil || cost <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range candidates(workspace, team, sprint) {
		st := s.states[k]
		if st == nil {
			continue
		}
		st.spent += cost
		s.emitGauges(st)
		if st.b.Enforcement != EnforcementOff {
			s.fireThresholds(st)
		}
	}
}

// emitGauges publishes the per-scope_id spend + utilization gauges, guarded
// so an unbounded scope_id space can't explode cardinality.
func (s *Service) emitGauges(st *budgetState) {
	if !s.guard.allow(string(st.b.Scope) + "|" + st.b.ScopeID) {
		return
	}
	metrics.SetBudgetSpent(string(st.b.Scope), st.b.ScopeID, st.spent)
	if st.b.LimitUSD > 0 {
		metrics.SetBudgetUtilization(string(st.b.Scope), st.b.ScopeID, st.spent/st.b.LimitUSD)
	}
}

// fireThresholds fires each not-yet-crossed alert threshold the current
// spend has reached. Caller holds s.mu.
func (s *Service) fireThresholds(st *budgetState) {
	if st.b.LimitUSD <= 0 {
		return
	}
	ratio := st.spent / st.b.LimitUSD
	for _, t := range st.b.AlertThresholds {
		if ratio >= t && !st.crossed[t] {
			st.crossed[t] = true
			metrics.BudgetThresholdCrossed(string(st.b.Scope))
			if s.alert != nil {
				s.alert(BudgetAlert{Budget: st.b, SpentUSD: st.spent, Threshold: t, Ratio: ratio})
			}
		}
	}
}

// Status returns the live state of every budget governing the given
// workspace/team/sprint triple.
func (s *Service) Status(workspace, team, sprint string) []Status {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Status
	for _, k := range candidates(workspace, team, sprint) {
		st := s.states[k]
		if st == nil {
			continue
		}
		b := st.b
		b.SpentUSD = st.spent
		var util float64
		if b.LimitUSD > 0 {
			util = st.spent / b.LimitUSD
		}
		out = append(out, Status{
			Budget:           b,
			UtilizationRatio: util,
			Decision:         b.decide(st.spent),
		})
	}
	return out
}
