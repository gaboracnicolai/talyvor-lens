// Package guardrails packages every safety check Lens performs on
// incoming prompts behind one unified API. PII detection, prompt-
// injection scoring, blocked-topic search, banned-word filtering, and
// arbitrary regex/custom-callback rules all flow through Engine.Check.
//
// Engine.Check is the only entry point proxy.serve() needs to call —
// it owns the pipeline ordering, the redacted-prompt threading, and
// the per-workspace policy lookup.
package guardrails

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
)

// GuardrailAction is the decision applied when a rule fires. Each
// stage (PII, injection, topic, word, custom) interprets the action
// according to its own semantics — `redact` only makes sense for
// stages that can rewrite the prompt, for example.
type GuardrailAction string

const (
	ActionBlock  GuardrailAction = "block"
	ActionRedact GuardrailAction = "redact"
	ActionWarn   GuardrailAction = "warn"
	ActionAllow  GuardrailAction = "allow"
)

type CustomRule struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Pattern string          `json:"pattern"`
	Action  GuardrailAction `json:"action"`
	Message string          `json:"message"`
}

type GuardrailPolicy struct {
	WorkspaceID      string          `json:"workspace_id"`
	EnablePII        bool            `json:"enable_pii"`
	EnableInjection  bool            `json:"enable_injection"`
	EnableTopics     bool            `json:"enable_topics"`
	BlockedTopics    []string        `json:"blocked_topics"`
	EnableWordFilter bool            `json:"enable_word_filter"`
	BlockedWords     []string        `json:"blocked_words"`
	PIIAction        GuardrailAction `json:"pii_action"`
	InjectionAction  GuardrailAction `json:"injection_action"`
	CustomRules      []CustomRule    `json:"custom_rules"`

	// ─── output-stage guardrails (Upgrade 13) ───
	// All default off (zero value): a workspace with no output config
	// behaves exactly as before. These run only when the engine's output
	// stage is enabled (LENS_GUARDRAILS_ENABLED) — see CheckOutput.
	OutputPIIAction       GuardrailAction `json:"output_pii_action,omitempty"`        // redact | block | "" (off)
	OutputValidateJSON    bool            `json:"output_validate_json,omitempty"`     // response content must be valid JSON
	OutputMaxLength       int             `json:"output_max_length,omitempty"`        // 0 = no limit (chars)
	OutputMustMatch       string          `json:"output_must_match,omitempty"`        // regex the response MUST match
	OutputMustNotMatch    string          `json:"output_must_not_match,omitempty"`    // regex the response must NOT match
	OutputValidationBlock bool            `json:"output_validation_block,omitempty"`  // validation failures block (else flag)
	BufferStreamForOutput bool            `json:"buffer_stream_for_output,omitempty"` // opt-in: buffer streamed responses so output guardrails can run (trades streaming for safety)
}

type Violation struct {
	Rule    string          `json:"rule"`
	Type    string          `json:"type"`
	Action  GuardrailAction `json:"action"`
	Message string          `json:"message"`
}

type GuardrailResult struct {
	Passed         bool            `json:"passed"`
	Action         GuardrailAction `json:"action,omitempty"`
	Violations     []Violation     `json:"violations"`
	RedactedPrompt string          `json:"redacted_prompt,omitempty"`
	RiskScore      float64         `json:"risk_score"`
}

// CustomGuardrail is the integration point for arbitrary checks that
// don't fit the regex / topic / word / PII model — e.g. an external
// classifier API, a model-side prompt-injection scorer, a content-
// policy classifier. Plug them in with Engine.AddCustomGuardrail.
type CustomGuardrail interface {
	Check(ctx context.Context, prompt string) GuardrailResult
	Name() string
}

// policyStore is the Postgres surface guardrail persistence needs (satisfied by
// *pgxpool.Pool). nil → the engine is in-memory only (tests), behaving exactly as
// before persistence was wired.
type policyStore interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Engine struct {
	pii       *pii.Detector
	injection *injection.Detector

	mu       sync.RWMutex
	custom   []CustomGuardrail
	policies map[string]*GuardrailPolicy

	// store persists per-workspace policies to guardrail_policies (0014). nil →
	// in-memory only. degraded is raised when custom policies could not be loaded
	// (startup) so the engine is serving defaults; loaded records whether a load
	// has EVER succeeded (so a later reload failure retains the last-good map
	// rather than flipping to degraded). All atomic so Check reads them lock-free.
	store    policyStore
	degraded atomic.Bool
	loaded   atomic.Bool

	// outputEnabled gates the output stage (CheckOutput). Off by default →
	// the input stage behaves exactly as today and no output guardrails run.
	// Atomic so the hot path reads it without the policy lock.
	outputEnabled atomic.Bool
}

func New(piiDetector *pii.Detector, injectionDetector *injection.Detector) *Engine {
	return &Engine{
		pii:       piiDetector,
		injection: injectionDetector,
		policies:  make(map[string]*GuardrailPolicy),
	}
}

// SetStore wires the Postgres persistence layer (guardrail_policies). A pool-less
// engine (tests) skips all DB I/O and stays in-memory only. Call once at startup,
// before Load — guarded against the typed-nil interface trap.
func (e *Engine) SetStore(pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	e.store = pool
}

// Degraded reports whether the engine is serving defaultPolicy for every workspace
// because custom policies could not be loaded at startup (DB down / a corrupt
// row). It is FALSE once any load has succeeded — a later reload failure retains
// the last-good policies (build-then-swap), which is not "degraded".
func (e *Engine) Degraded() bool { return e != nil && e.degraded.Load() }

// defaultPolicy is what every workspace gets until SetPolicy is called.
// "PII redact, injection block, everything enabled" is the safest
// production default — privacy violations get scrubbed silently,
// adversarial prompts get rejected outright.
func defaultPolicy() *GuardrailPolicy {
	return &GuardrailPolicy{
		EnablePII:        true,
		EnableInjection:  true,
		EnableTopics:     true,
		EnableWordFilter: true,
		PIIAction:        ActionRedact,
		InjectionAction:  ActionBlock,
	}
}

const upsertPolicySQL = `INSERT INTO guardrail_policies (workspace_id, policy)
VALUES ($1, $2)
ON CONFLICT (workspace_id) DO UPDATE SET policy = $2, updated_at = NOW()`

// SetPolicy stores a policy in the in-memory map AND, when a store is wired,
// persists it to guardrail_policies so it survives restart and propagates to other
// replicas on their next reload. The change takes effect on the very next Check
// call — no per-request caching. Map-first (the local node is immediately correct,
// mirroring wsManager.SetLoggingPolicy); a returned error means the policy is live
// locally but did NOT persist (the caller surfaces it so the admin knows it won't
// survive a restart). The jsonb param goes through dbjson.Marshal — text-encoded,
// pgbouncer-safe (#133), never a raw []byte.
func (e *Engine) SetPolicy(ctx context.Context, wsID string, policy GuardrailPolicy) error {
	if wsID == "" {
		wsID = "default"
	}
	policy.WorkspaceID = wsID
	stored := policy
	e.mu.Lock()
	e.policies[wsID] = &stored
	e.mu.Unlock()

	if e.store == nil {
		return nil // in-memory only
	}
	doc, err := dbjson.Marshal(policy)
	if err != nil {
		return fmt.Errorf("guardrails: marshal policy for %q: %w", wsID, err)
	}
	if _, err := e.store.Exec(ctx, upsertPolicySQL, wsID, doc); err != nil {
		return fmt.Errorf("guardrails: persist policy for %q (live locally, NOT durable): %w", wsID, err)
	}
	return nil
}

// GetPolicy returns a copy of the workspace's policy or the safe
// default when no policy was registered. Returning a copy keeps the
// caller from mutating engine state through the returned pointer.
func (e *Engine) GetPolicy(wsID string) *GuardrailPolicy {
	if wsID == "" {
		wsID = "default"
	}
	e.mu.RLock()
	p, ok := e.policies[wsID]
	e.mu.RUnlock()
	if !ok {
		return defaultPolicy()
	}
	cp := *p
	return &cp
}

// AddCustomGuardrail registers an external check. Thread-safe.
func (e *Engine) AddCustomGuardrail(g CustomGuardrail) {
	e.mu.Lock()
	e.custom = append(e.custom, g)
	e.mu.Unlock()
}

// Check runs every enabled rule against the prompt in spec order:
// PII, injection, topics, word filter, custom rules, custom guardrails.
// Blocking violations short-circuit so we don't waste cycles on
// downstream checks once we know we're returning 4xx anyway.
func (e *Engine) Check(ctx context.Context, wsID, prompt string, body []byte) GuardrailResult {
	policy := e.GetPolicy(wsID)
	result := GuardrailResult{Passed: true}
	current := prompt

	// 1. PII — redact-or-block depending on policy.
	if policy.EnablePII && e.pii != nil {
		piiRes := e.pii.Detect(current)
		if piiRes.WasRedacted {
			v := Violation{
				Rule:    "pii",
				Type:    "pii",
				Action:  policy.PIIAction,
				Message: "PII detected: " + strings.Join(piiRes.Types, ", "),
			}
			result.Violations = append(result.Violations, v)
			switch policy.PIIAction {
			case ActionBlock:
				result.Passed = false
				result.Action = ActionBlock
				return result
			case ActionRedact:
				current = piiRes.Redacted
				result.RedactedPrompt = current
			case ActionWarn:
				// metadata only; downstream sees the original prompt.
			}
		}
	}

	// 2. Injection — risk-score band drives the action.
	if policy.EnableInjection && e.injection != nil {
		ir := e.injection.Detect(current)
		if ir.RiskScore > result.RiskScore {
			result.RiskScore = ir.RiskScore
		}
		switch {
		case ir.RiskScore >= 0.7:
			v := Violation{
				Rule:    "injection",
				Type:    "injection",
				Action:  policy.InjectionAction,
				Message: fmt.Sprintf("Injection risk %.2f", ir.RiskScore),
			}
			result.Violations = append(result.Violations, v)
			if policy.InjectionAction == ActionBlock {
				result.Passed = false
				result.Action = ActionBlock
				return result
			}
		case ir.RiskScore >= 0.3:
			// Warn-only band — log the signal but never block.
			result.Violations = append(result.Violations, Violation{
				Rule:    "injection_warn",
				Type:    "injection",
				Action:  ActionWarn,
				Message: fmt.Sprintf("Injection risk %.2f (below block threshold)", ir.RiskScore),
			})
		}
	}

	// 3. Topic filter — substring match, case-insensitive. Blocks.
	if policy.EnableTopics {
		lower := strings.ToLower(current)
		for _, topic := range policy.BlockedTopics {
			if topic == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(topic)) {
				result.Violations = append(result.Violations, Violation{
					Rule:    "topic",
					Type:    "topic",
					Action:  ActionBlock,
					Message: "Topic not allowed: " + topic,
				})
				result.Passed = false
				result.Action = ActionBlock
				return result
			}
		}
	}

	// 4. Word filter — replace with [FILTERED], continue.
	if policy.EnableWordFilter {
		for _, word := range policy.BlockedWords {
			if word == "" {
				continue
			}
			re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(word))
			if re.MatchString(current) {
				current = re.ReplaceAllString(current, "[FILTERED]")
				result.RedactedPrompt = current
				result.Violations = append(result.Violations, Violation{
					Rule:    "word_filter",
					Type:    "word_filter",
					Action:  ActionRedact,
					Message: "Blocked word filtered: " + word,
				})
			}
		}
	}

	// 5. Custom regex rules — block / redact / warn.
	for _, rule := range policy.CustomRules {
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			// Skip malformed patterns rather than failing the whole
			// check — bad config shouldn't take production offline.
			continue
		}
		if re.MatchString(current) {
			result.Violations = append(result.Violations, Violation{
				Rule:    rule.ID,
				Type:    "custom",
				Action:  rule.Action,
				Message: rule.Message,
			})
			switch rule.Action {
			case ActionBlock:
				result.Passed = false
				result.Action = ActionBlock
				return result
			case ActionRedact:
				current = re.ReplaceAllString(current, "[REDACTED]")
				result.RedactedPrompt = current
			}
		}
	}

	// 6. Plug-in custom guardrails registered via AddCustomGuardrail.
	e.mu.RLock()
	custom := append([]CustomGuardrail{}, e.custom...)
	e.mu.RUnlock()
	for _, g := range custom {
		cr := g.Check(ctx, current)
		result.Violations = append(result.Violations, cr.Violations...)
		if cr.RiskScore > result.RiskScore {
			result.RiskScore = cr.RiskScore
		}
		if cr.RedactedPrompt != "" && cr.RedactedPrompt != current {
			current = cr.RedactedPrompt
			result.RedactedPrompt = current
		}
		if cr.Action == ActionBlock {
			result.Passed = false
			result.Action = ActionBlock
			return result
		}
	}

	return result
}

// ─── output stage (Upgrade 13) ───

// SetOutputEnabled toggles the output stage (wired from
// LENS_GUARDRAILS_ENABLED). When off, CheckOutput is a no-op.
func (e *Engine) SetOutputEnabled(b bool) {
	if e != nil {
		e.outputEnabled.Store(b)
	}
}

// OutputEnabled reports whether the output stage is on.
func (e *Engine) OutputEnabled() bool { return e != nil && e.outputEnabled.Load() }

// ShouldBufferStream reports whether a streamed response for this workspace
// should be buffered so output guardrails can run (the opt-in that trades
// streaming for output safety). False unless the output stage is on AND the
// workspace opted in.
func (e *Engine) ShouldBufferStream(wsID string) bool {
	if !e.OutputEnabled() {
		return false
	}
	return e.GetPolicy(wsID).BufferStreamForOutput
}

const deletePolicySQL = `DELETE FROM guardrail_policies WHERE workspace_id = $1`

// DeletePolicy removes a workspace's policy so it reverts to the default — from the
// in-memory map AND, when a store is wired, the DB row, so a reset-to-default is
// durable and does not resurrect on the next reload.
func (e *Engine) DeletePolicy(ctx context.Context, wsID string) error {
	if wsID == "" {
		wsID = "default"
	}
	e.mu.Lock()
	delete(e.policies, wsID)
	e.mu.Unlock()

	if e.store == nil {
		return nil
	}
	if _, err := e.store.Exec(ctx, deletePolicySQL, wsID); err != nil {
		return fmt.Errorf("guardrails: delete persisted policy for %q: %w", wsID, err)
	}
	return nil
}

const loadPoliciesSQL = `SELECT workspace_id, policy FROM guardrail_policies`

// Load rebuilds the in-memory policy map from guardrail_policies, build-then-swap
// (the proven U7b shape): a fresh map is assembled OUTSIDE the lock, then published
// with a single pointer swap under the write lock — readers never see a partial
// map, and a failed build never swaps. ONLY e.policies is swapped; e.custom and
// e.outputEnabled are untouched. No-op when no store is wired.
//
// FAIL DIRECTION (this is a SECURITY control):
//   - (1) RELOAD failure (query/scan/rows error) once a load has succeeded → the
//     last-good map is RETAINED (no swap), not flipped to default. Warn, not degraded.
//   - (2) A single unparseable/corrupt policy row → the WHOLE build FAILS (the bad
//     workspace_id is logged loudly). A bad row is NEVER skipped: skipping would
//     silently downgrade that one tenant to defaultPolicy, which can be LOOSER than
//     its intended tightening (default Redact vs a workspace's Block; no blocklist
//     vs its blocklist). On reload the last-good map survives; on startup it stays
//     empty → degraded (below).
//   - (3) STARTUP load failure (no prior successful load) → the map stays empty so
//     GetPolicy serves defaultPolicy for every workspace (PII redact + injection
//     block stay ON), BUT the engine raises Degraded() + a loud ERROR so the
//     downgrade is VISIBLE, not silent. NOT full fail-closed/block-all.
func (e *Engine) Load(ctx context.Context) error {
	if e.store == nil {
		return nil
	}
	rows, err := e.store.Query(ctx, loadPoliciesSQL)
	if err != nil {
		return e.loadFailed(fmt.Errorf("guardrails: load policies: %w", err), "")
	}
	defer rows.Close()

	next := make(map[string]*GuardrailPolicy)
	for rows.Next() {
		var wsID string
		var doc []byte
		if err := rows.Scan(&wsID, &doc); err != nil {
			return e.loadFailed(fmt.Errorf("guardrails: scan policy row: %w", err), "")
		}
		var p GuardrailPolicy
		if err := json.Unmarshal(doc, &p); err != nil {
			// (2) FAIL THE WHOLE BUILD — never skip-the-row.
			return e.loadFailed(fmt.Errorf("guardrails: unmarshal policy for %q: %w", wsID, err), wsID)
		}
		if p.WorkspaceID == "" {
			p.WorkspaceID = wsID
		}
		stored := p
		next[wsID] = &stored
	}
	if err := rows.Err(); err != nil {
		return e.loadFailed(fmt.Errorf("guardrails: load policies rows: %w", err), "")
	}

	// Success — publish the fresh map, clear degraded, mark loaded.
	e.mu.Lock()
	e.policies = next
	e.mu.Unlock()
	e.degraded.Store(false)
	e.loaded.Store(true)
	return nil
}

// loadFailed centralizes the fail direction: build-then-swap means we never
// published a partial map, so the existing map (last-good on reload, empty on a
// cold start) is already what readers see. We only raise Degraded()+ERROR when we
// have NEVER successfully loaded (startup → serving defaults); a reload failure
// over a good map is a WARN and stays non-degraded.
func (e *Engine) loadFailed(err error, badWorkspaceID string) error {
	if e.loaded.Load() {
		slog.Warn("guardrails: reload failed — retaining last-good policies",
			slog.String("workspace_id", badWorkspaceID), slog.String("err", err.Error()))
		return err
	}
	e.degraded.Store(true)
	slog.Error("GUARDRAILS DEGRADED: custom policies not loaded, serving defaults (PII redact + injection block stay ON for all workspaces)",
		slog.String("workspace_id", badWorkspaceID), slog.String("err", err.Error()))
	return err
}

// Reload rebuilds the policy map from the store — public + directly callable (the
// U7c test seam). Equivalent to Load.
func (e *Engine) Reload(ctx context.Context) error { return e.Load(ctx) }

// StartRefresh runs Reload on a fixed interval until ctx is cancelled, bounding
// cross-replica staleness of custom guardrail policies. A transient error keeps the
// last-good map (build-then-swap) and the next tick retries. No-op without a store.
// NOT leader-gated — every replica refreshes its own cache. Mirrors
// wsManager.StartRefresh.
func (e *Engine) StartRefresh(ctx context.Context, interval time.Duration) {
	if e == nil || e.store == nil {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := e.Reload(ctx); err != nil {
					slog.Warn("guardrails: scheduled reload failed (last-good kept)", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

// CheckOutput runs the OUTPUT-stage guardrails over a response's content
// (the assistant text, extracted by the caller). A no-op returning Passed
// when the output stage is disabled — so off = behaves as today.
//
// On a redact action, RedactedPrompt carries the redacted content (the
// caller re-injects it into the response). On block, Passed=false — the
// caller rejects the response (the upstream call already ran, so spend is
// still recorded; the offending content is just never returned/cached).
func (e *Engine) CheckOutput(_ context.Context, wsID, content string) GuardrailResult {
	if e == nil || !e.outputEnabled.Load() {
		return GuardrailResult{Passed: true}
	}
	return e.evalOutput(e.GetPolicy(wsID), content)
}

// CheckOutputPreview runs the output guardrails IGNORING the enabled flag —
// for the dry-run test endpoint, so users can preview what would trigger even
// before they turn the output stage on. Never used on the request path.
func (e *Engine) CheckOutputPreview(wsID, content string) GuardrailResult {
	if e == nil {
		return GuardrailResult{Passed: true}
	}
	return e.evalOutput(e.GetPolicy(wsID), content)
}

func (e *Engine) evalOutput(policy *GuardrailPolicy, content string) GuardrailResult {
	result := GuardrailResult{Passed: true}
	current := content

	// Output PII — redact or block.
	if policy.OutputPIIAction != "" && e.pii != nil {
		r := e.pii.Detect(current)
		if r.WasRedacted {
			result.Violations = append(result.Violations, Violation{
				Rule: "output_pii", Type: "pii", Action: policy.OutputPIIAction,
				Message: "PII in response: " + strings.Join(r.Types, ", "),
			})
			switch policy.OutputPIIAction {
			case ActionBlock:
				result.Passed = false
				result.Action = ActionBlock
				return result
			case ActionRedact:
				current = r.Redacted
				result.RedactedPrompt = current
			}
		}
	}

	// Output validation — JSON validity, length, regex. The action is block
	// when OutputValidationBlock, else warn (flag). The first blocking
	// failure short-circuits.
	valAction := ActionWarn
	if policy.OutputValidationBlock {
		valAction = ActionBlock
	}
	addVal := func(msg string) bool { // returns true when it blocks
		result.Violations = append(result.Violations, Violation{
			Rule: "output_validation", Type: "output_validation", Action: valAction, Message: msg,
		})
		if valAction == ActionBlock {
			result.Passed = false
			result.Action = ActionBlock
			return true
		}
		return false
	}
	if policy.OutputValidateJSON && !json.Valid([]byte(current)) {
		if addVal("response content is not valid JSON") {
			return result
		}
	}
	if policy.OutputMaxLength > 0 && len(current) > policy.OutputMaxLength {
		if addVal(fmt.Sprintf("response exceeds max length %d", policy.OutputMaxLength)) {
			return result
		}
	}
	if policy.OutputMustMatch != "" {
		if re, err := regexp.Compile(policy.OutputMustMatch); err == nil && !re.MatchString(current) {
			if addVal("response does not match the required pattern") {
				return result
			}
		}
	}
	if policy.OutputMustNotMatch != "" {
		if re, err := regexp.Compile(policy.OutputMustNotMatch); err == nil && re.MatchString(current) {
			if addVal("response matches a forbidden pattern") {
				return result
			}
		}
	}
	return result
}
