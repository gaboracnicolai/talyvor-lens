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
	"fmt"
	"regexp"
	"strings"
	"sync"

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

type Engine struct {
	pii       *pii.Detector
	injection *injection.Detector

	mu       sync.RWMutex
	custom   []CustomGuardrail
	policies map[string]*GuardrailPolicy
}

func New(piiDetector *pii.Detector, injectionDetector *injection.Detector) *Engine {
	return &Engine{
		pii:       piiDetector,
		injection: injectionDetector,
		policies:  make(map[string]*GuardrailPolicy),
	}
}

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

// SetPolicy stores a policy in the in-memory map. The change takes
// effect on the very next Check call — no per-request caching.
func (e *Engine) SetPolicy(_ context.Context, wsID string, policy GuardrailPolicy) {
	if wsID == "" {
		wsID = "default"
	}
	policy.WorkspaceID = wsID
	stored := policy
	e.mu.Lock()
	e.policies[wsID] = &stored
	e.mu.Unlock()
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
