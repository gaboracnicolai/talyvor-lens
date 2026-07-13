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

	"github.com/talyvor/lens/internal/catalog"
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
	// WorkspaceID is the workspace whose request tripped the threshold (SEC-7).
	// Carried so the Track emitter can attribute the alert to the right workspace
	// (Track credits issues by workspace_id + feature). Additive — the NATS payload
	// gains a field, which existing consumers ignore. Empty for a monitor-triggered
	// alert (aggregate, no single workspace).
	WorkspaceID string `json:"workspace_id"`
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
	emitter  *Emitter // SEC-7: outbound Track spend-alert webhook (nil = disabled no-op)
}

// SetEmitter wires the Track spend-alert webhook emitter (SEC-7). Optional — a nil
// emitter (or one with no URL/secret) is a total no-op. The emit is async and can
// NEVER block a serve (see Emitter's security invariant).
func (a *AlertManager) SetEmitter(e *Emitter) { a.emitter = e }

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

// costUSD returns the realized USD cost for the request, pricing the model
// from the catalog (the single source of truth — Upgrade 16). Unknown models
// cost 0 — we'd rather miss an alert than fire a false one off bad data.
func costUSD(model string, inputTokens, outputTokens int) float64 {
	inP, outP, ok := catalog.Price(model)
	if !ok {
		return 0
	}
	return (float64(inputTokens)*inP + float64(outputTokens)*outP) / 1_000_000
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
	// Catalog is authoritative for known models (Upgrade 16) — this also
	// resolves dated-snapshot aliases to their canonical model's provider.
	if m, ok := catalog.Get(model); ok {
		return m.Provider
	}
	// Unknown to the catalog — derive the provider from the name prefix,
	// exactly as before. Bedrock model IDs ("anthropic.claude-…") are checked
	// before the generic claude- branch so a Bedrock-billed row never gets
	// logged as a direct Anthropic call.
	switch {
	case strings.HasPrefix(model, "anthropic."):
		return "bedrock"
	case strings.HasPrefix(model, "claude-"):
		return "anthropic"
	case strings.HasPrefix(model, "gemini-"):
		return "google"
	// vLLM is self-hosted; the user-facing model name is whatever the
	// operator loaded, namespaced with "vllm/" to keep cache keys disjoint.
	case strings.HasPrefix(model, "vllm/"):
		return "vllm"
	case strings.HasPrefix(model, "mistral-"), strings.HasPrefix(model, "open-mistral-"):
		return "mistral"
	case strings.HasPrefix(model, "llama-"), strings.HasPrefix(model, "mixtral-"), strings.HasPrefix(model, "gemma"):
		return "groq"
	}
	return "unknown"
}

const insertTokenEventSQL = `INSERT INTO token_events
  (workspace_id, provider, model, input_tokens, output_tokens, team, sprint_id, feature, cost_usd, prompt_text, session_id, request_id, modality, cost_estimated, distill_method)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

// RecordSpend takes a `prompt` (or already-redacted equivalent) so the
// cache warmer can later JOIN prompt_embeddings against token_events to
// recover the prompt text it needs to re-warm popular patterns. The
// session_id and request_id arguments are persisted so audit exports
// can correlate spend rows back to a chat session and HTTP request.
//
// workspaceID and sprintID land in token_events alongside the existing
// team so spend can be summed per workspace / team / sprint from this
// single billing write. CORRECTNESS NOTE: workspaceID was previously not
// persisted at all (the column fell back to its 'default'), so the
// per-workspace spend cap was summing globally; passing it here makes that
// cap — and the new workspace-scoped budgets — truly per-workspace. See
// migration 0028.
//
// modality + estimated land on this same single write (migration 0029) so
// budgets/forecast/anomaly/ROI can reflect image cost. modality is the
// canonical label ("text" / "image" / "image,audio" …); estimated is true
// when the cost is a documented estimate rather than exact accounting
// (multimodal input tokens are estimated).
func (a *AlertManager) RecordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool) error {
	// Non-distilled traffic: distill_method is '' (the column default). Existing
	// callers are unchanged.
	return a.recordSpend(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, prompt, sessionID, requestID, modality, estimated, "")
}

// RecordSpendWithDistill is RecordSpend plus the DISTILL method attribution
// written to token_events.distill_method: "convert" for a distilled request's
// (lower-count) spend row, or "vision_ocr" for the OCR sub-call's OWN cost row.
// Additive — it shares the exact billing/alert path; only the distill_method tag
// differs, so non-distilled traffic and all existing callers are untouched.
func (a *AlertManager) RecordSpendWithDistill(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error {
	return a.recordSpend(ctx, workspaceID, team, sprint, feature, model, inputTokens, outputTokens, prompt, sessionID, requestID, modality, estimated, distillMethod)
}

func (a *AlertManager) recordSpend(ctx context.Context, workspaceID, team, sprint, feature, model string, inputTokens, outputTokens int, prompt, sessionID, requestID, modality string, estimated bool, distillMethod string) error {
	cost := costUSD(model, inputTokens, outputTokens)
	provider := providerForModel(model)
	if modality == "" {
		modality = "text"
	}

	if _, err := a.pool.Exec(ctx, insertTokenEventSQL,
		workspaceID, provider, model, inputTokens, outputTokens, team, sprint, feature, cost, prompt, sessionID, requestID, modality, estimated, distillMethod,
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
		a.evaluateRule(ctx, rule, spend, workspaceID)
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
func (a *AlertManager) evaluateRule(ctx context.Context, rule SpendRule, spend float64, workspaceID string) {
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
			Level:       AlertCritical,
			Team:        rule.Team,
			Feature:     rule.Feature,
			Message:     "Critical spend threshold exceeded",
			SpendUSD:    spend,
			Threshold:   rule.CriticalUSD,
			CreatedAt:   time.Now().UTC(),
			WorkspaceID: workspaceID,
		})
	}
	if spend > rule.WarningUSD {
		_ = a.fireAlert(ctx, Alert{
			Level:       AlertWarning,
			Team:        rule.Team,
			Feature:     rule.Feature,
			Message:     "Warning spend threshold exceeded",
			SpendUSD:    spend,
			Threshold:   rule.WarningUSD,
			CreatedAt:   time.Now().UTC(),
			WorkspaceID: workspaceID,
		})
	}
}

func (a *AlertManager) fireAlert(_ context.Context, alert Alert) error {
	data, err := json.Marshal(alert)
	if err != nil {
		return err
	}
	// SEC-7: emit to Track's webhook — async, fire-and-forget, NEVER blocks (see
	// Emitter). Independent of and BEFORE the NATS publish, so a spend alert reaches
	// Track even when NATS is down; a nil/disabled emitter is a no-op.
	a.emitter.Emit(alert)

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
