package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

type AlertLevel string

const (
	AlertWarning  AlertLevel = "warning"
	AlertCritical AlertLevel = "critical"
)

type Alert struct {
	Level     AlertLevel `json:"level"`
	Feature   string     `json:"feature"`
	Team      string     `json:"team"`
	Message   string     `json:"message"`
	SpendUSD  float64    `json:"spend_usd"`
	Threshold float64    `json:"threshold_usd"`
	CreatedAt time.Time  `json:"created_at"`
}

type CircuitState string

const (
	CircuitClosed CircuitState = "closed"
	CircuitOpen   CircuitState = "open"
)

type SpendRule struct {
	ID          string
	Team        string
	Feature     string
	WindowHours int
	WarningUSD  float64
	CriticalUSD float64
	CircuitUSD  float64
}

// pgxDB is the subset of *pgxpool.Pool that AlertManager needs. Defined so
// tests can substitute pgxmock without a real database.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type AlertManager struct {
	pool     pgxDB
	nc       *nats.Conn
	rules    []SpendRule
	circuits map[string]CircuitState
	mu       sync.RWMutex
}

func New(pool *pgxpool.Pool, nc *nats.Conn, rules []SpendRule) *AlertManager {
	return newAlertManager(pool, nc, rules)
}

func newAlertManager(pool pgxDB, nc *nats.Conn, rules []SpendRule) *AlertManager {
	return &AlertManager{
		pool:     pool,
		nc:       nc,
		rules:    rules,
		circuits: make(map[string]CircuitState),
	}
}

// modelPrice holds per-million-token USD pricing for one model.
type modelPrice struct {
	inputPerMillion  float64
	outputPerMillion float64
}

// modelPrices is the per-model USD price table from the spec. Numbers here
// are authoritative — tests assert exact arithmetic, so do not round.
var modelPrices = map[string]modelPrice{
	// OpenAI
	"gpt-4o":            {2.50, 10.00},
	"gpt-4o-mini":       {0.15, 0.60},
	"gpt-4.1-nano":      {0.10, 0.40},
	"gpt-5.4":           {5.00, 20.00},
	"gpt-5.4-mini":      {0.50, 2.00},
	"gpt-4.1":           {2.00, 8.00},
	"gpt-4.1-mini":      {0.40, 1.60},
	// Anthropic
	"claude-opus-4-5":   {15.00, 75.00},
	"claude-sonnet-4-5": {3.00, 15.00},
	"claude-haiku-4-5":  {0.80, 4.00},
	"claude-opus-4-6":   {15.00, 75.00},
	"claude-sonnet-4-6": {3.00, 15.00},
	"claude-haiku-4-6":  {0.80, 4.00},
	// Google Gemini
	"gemini-2.5-pro":   {1.25, 10.00},
	"gemini-2.5-flash": {0.075, 0.30},
	"gemini-2.0-flash": {0.10, 0.40},
	"gemini-1.5-pro":   {1.25, 5.00},
	"gemini-1.5-flash": {0.075, 0.30},
}

// costUSD returns the realized USD cost for the request. Unknown models
// cost 0 — we'd rather miss an alert than fire a false one off bad data.
func costUSD(model string, inputTokens, outputTokens int) float64 {
	p, ok := modelPrices[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)*p.inputPerMillion + float64(outputTokens)*p.outputPerMillion) / 1_000_000
}

// CostUSD is the exported entry point on the per-million-token price table.
// Other packages (e.g. the A/B tester) call this so there's a single source
// of truth for model pricing across the system.
func CostUSD(model string, inputTokens, outputTokens int) float64 {
	return costUSD(model, inputTokens, outputTokens)
}

// providerForModel derives the provider name from the model name. Used so
// the (provider, model) pair stays consistent in token_events even when the
// caller only supplies the model.
func providerForModel(model string) string {
	if strings.HasPrefix(model, "claude-") {
		return "anthropic"
	}
	if strings.HasPrefix(model, "gemini-") {
		return "google"
	}
	if _, ok := modelPrices[model]; ok {
		return "openai"
	}
	return "unknown"
}

const insertTokenEventSQL = `INSERT INTO token_events
  (provider, model, input_tokens, output_tokens, team, feature, cost_usd, prompt_text)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// RecordSpend takes a `prompt` (or already-redacted equivalent) so the
// cache warmer can later JOIN prompt_embeddings against token_events to
// recover the prompt text it needs to re-warm popular patterns.
func (a *AlertManager) RecordSpend(ctx context.Context, team, feature, model string, inputTokens, outputTokens int, prompt string) error {
	cost := costUSD(model, inputTokens, outputTokens)
	provider := providerForModel(model)

	if _, err := a.pool.Exec(ctx, insertTokenEventSQL,
		provider, model, inputTokens, outputTokens, team, feature, cost, prompt,
	); err != nil {
		return fmt.Errorf("alerts: insert token_event: %w", err)
	}

	for _, rule := range a.rules {
		if rule.Team != team || rule.Feature != feature {
			continue
		}
		spend, err := a.windowSpend(ctx, rule)
		if err != nil {
			slog.Warn("alerts: window spend query failed",
				slog.String("team", team),
				slog.String("feature", feature),
				slog.String("err", err.Error()),
			)
			continue
		}
		a.evaluateRule(ctx, rule, spend)
	}
	return nil
}

func (a *AlertManager) windowSpend(ctx context.Context, rule SpendRule) (float64, error) {
	// INTERVAL is parameterized only via a literal hours value because pgx
	// can't bind a SQL interval cleanly — the rule's WindowHours comes from
	// our own config, not user input, so interpolation here is safe.
	q := fmt.Sprintf(`SELECT COALESCE(SUM(cost_usd), 0)
FROM token_events
WHERE team = $1 AND feature = $2
  AND created_at > NOW() - INTERVAL '%d hours'`, rule.WindowHours)
	var spend float64
	if err := a.pool.QueryRow(ctx, q, rule.Team, rule.Feature).Scan(&spend); err != nil {
		return 0, err
	}
	return spend, nil
}

// evaluateRule fires alerts and toggles the circuit breaker based on the
// rule's tier thresholds. Each tier fires independently — warning is still
// emitted when critical is also true so dashboards see the full ladder.
func (a *AlertManager) evaluateRule(ctx context.Context, rule SpendRule, spend float64) {
	key := circuitKey(rule.Team, rule.Feature)

	if spend > rule.CircuitUSD {
		a.mu.Lock()
		prev := a.circuits[key]
		a.circuits[key] = CircuitOpen
		a.mu.Unlock()
		if prev != CircuitOpen {
			slog.Warn("alerts: circuit opened",
				slog.String("team", rule.Team),
				slog.String("feature", rule.Feature),
				slog.Float64("spend_usd", spend),
				slog.Float64("threshold_usd", rule.CircuitUSD),
			)
		}
	}

	if spend > rule.CriticalUSD {
		_ = a.fireAlert(ctx, Alert{
			Level:     AlertCritical,
			Team:      rule.Team,
			Feature:   rule.Feature,
			Message:   "Critical spend threshold exceeded",
			SpendUSD:  spend,
			Threshold: rule.CriticalUSD,
			CreatedAt: time.Now().UTC(),
		})
	}
	if spend > rule.WarningUSD {
		_ = a.fireAlert(ctx, Alert{
			Level:     AlertWarning,
			Team:      rule.Team,
			Feature:   rule.Feature,
			Message:   "Warning spend threshold exceeded",
			SpendUSD:  spend,
			Threshold: rule.WarningUSD,
			CreatedAt: time.Now().UTC(),
		})
	}
}

func (a *AlertManager) fireAlert(_ context.Context, alert Alert) error {
	data, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	if a.nc == nil {
		return errors.New("alerts: no NATS connection")
	}
	// Publish to the level-specific subject and the all-subject for dashboards.
	if err := a.nc.Publish("lens.alerts."+string(alert.Level), data); err != nil {
		slog.Warn("alerts: publish failed", slog.String("err", err.Error()))
	}
	if err := a.nc.Publish("lens.alerts.all", data); err != nil {
		slog.Warn("alerts: publish-all failed", slog.String("err", err.Error()))
	}

	switch alert.Level {
	case AlertCritical:
		slog.Error("alerts: critical spend",
			slog.String("team", alert.Team),
			slog.String("feature", alert.Feature),
			slog.Float64("spend_usd", alert.SpendUSD),
			slog.Float64("threshold_usd", alert.Threshold),
		)
	default:
		slog.Warn("alerts: warning spend",
			slog.String("team", alert.Team),
			slog.String("feature", alert.Feature),
			slog.Float64("spend_usd", alert.SpendUSD),
			slog.Float64("threshold_usd", alert.Threshold),
		)
	}
	return nil
}

func (a *AlertManager) IsCircuitOpen(team, feature string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.circuits[circuitKey(team, feature)] == CircuitOpen
}

func (a *AlertManager) GetDowngradeModel(provider, _ string) string {
	switch provider {
	case "anthropic":
		return "claude-haiku-4-5"
	default:
		return "gpt-4o-mini"
	}
}

func (a *AlertManager) ResetCircuit(team, feature string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.circuits, circuitKey(team, feature))
}

// openCircuitForTest is an unexported helper so tests can put a circuit in
// the open state without setting up the full RecordSpend pipeline.
func (a *AlertManager) openCircuitForTest(team, feature string) {
	a.mu.Lock()
	a.circuits[circuitKey(team, feature)] = CircuitOpen
	a.mu.Unlock()
}

// OpenCircuit lets callers (admin endpoints, ops tooling) trip a circuit
// breaker without waiting for spend to cross the threshold.
func (a *AlertManager) OpenCircuit(team, feature string) {
	a.mu.Lock()
	a.circuits[circuitKey(team, feature)] = CircuitOpen
	a.mu.Unlock()
}

// CircuitStates returns a copy of the current circuit-breaker states,
// keyed by "team:feature". Each value is the string form of CircuitState.
func (a *AlertManager) CircuitStates() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]string, len(a.circuits))
	for k, v := range a.circuits {
		out[k] = string(v)
	}
	return out
}

// Rules returns a copy of the configured spend rules. Rules are
// effectively immutable after construction so we don't need a lock here.
func (a *AlertManager) Rules() []SpendRule {
	out := make([]SpendRule, len(a.rules))
	copy(out, a.rules)
	return out
}

func circuitKey(team, feature string) string {
	return team + ":" + feature
}

const monitorInterval = 5 * time.Minute

// StartMonitor spawns a background goroutine that re-evaluates each rule
// every 5 minutes and closes circuits where the rolling-window spend has
// dropped below CircuitUSD. Exits when ctx is cancelled.
func (a *AlertManager) StartMonitor(ctx context.Context) {
	go a.monitorLoop(ctx, monitorInterval)
}

func (a *AlertManager) monitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.evaluateAllRules(ctx)
		}
	}
}

func (a *AlertManager) evaluateAllRules(ctx context.Context) {
	for _, rule := range a.rules {
		spend, err := a.windowSpend(ctx, rule)
		if err != nil {
			slog.Warn("alerts: monitor window spend failed",
				slog.String("rule_id", rule.ID),
				slog.String("err", err.Error()),
			)
			continue
		}
		key := circuitKey(rule.Team, rule.Feature)

		a.mu.Lock()
		state := a.circuits[key]
		switch {
		case state == CircuitOpen && spend <= rule.CircuitUSD:
			delete(a.circuits, key)
			slog.Info("alerts: circuit closed by monitor",
				slog.String("team", rule.Team),
				slog.String("feature", rule.Feature),
				slog.Float64("spend_usd", spend),
			)
		case state != CircuitOpen && spend > rule.CircuitUSD:
			a.circuits[key] = CircuitOpen
			slog.Warn("alerts: circuit opened by monitor",
				slog.String("team", rule.Team),
				slog.String("feature", rule.Feature),
				slog.Float64("spend_usd", spend),
			)
		}
		a.mu.Unlock()
	}
}
