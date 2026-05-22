package budget

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BudgetPolicy struct {
	MaxOutputTokens int
	MaxInputTokens  int
	MaxTotalTokens  int
}

type EnforcementResult struct {
	Original  BudgetPolicy
	Applied   BudgetPolicy
	Rewritten bool
	Reason    string
}

// pgxDB is the subset of *pgxpool.Pool the enforcer needs. Tests pass nil
// so they exercise only the pure rewriting logic — `GetPolicy` returns
// the defaults in that case.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Enforcer struct {
	pool     pgxDB
	defaults BudgetPolicy
}

func New(pool *pgxpool.Pool, defaults BudgetPolicy) *Enforcer {
	// Avoid the typed-nil interface trap so GetPolicy can compare to nil.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Enforcer{pool: db, defaults: defaults}
}

const getWorkspaceBudgetSQL = `SELECT max_output_tokens, max_input_tokens
FROM workspaces
WHERE id = $1`

// GetPolicy looks up the workspace's per-tenant budget and falls back to
// the enforcer's defaults for any limit the workspace leaves at 0.
// DB errors quietly degrade to defaults — we'd rather under-enforce than
// fail the request from a budget lookup.
func (e *Enforcer) GetPolicy(ctx context.Context, wsID string) BudgetPolicy {
	policy := e.defaults
	if e.pool == nil || wsID == "" {
		return policy
	}
	var maxOut, maxIn int
	if err := e.pool.QueryRow(ctx, getWorkspaceBudgetSQL, wsID).Scan(&maxOut, &maxIn); err != nil {
		return policy
	}
	if maxOut > 0 {
		policy.MaxOutputTokens = maxOut
	}
	if maxIn > 0 {
		policy.MaxInputTokens = maxIn
	}
	return policy
}

// EnforceOnBody rewrites the JSON body so its max_tokens field complies
// with the effective workspace policy. Returns the (possibly modified)
// body, an EnforcementResult describing what changed, and a parse error
// if the body wasn't valid JSON.
//
// Rules:
//   - Policy MaxOutputTokens == 0 → never rewrite.
//   - max_tokens missing → inject the policy value.
//   - max_tokens > policy → reduce to policy value.
//   - max_tokens <= policy → leave the body untouched.
func (e *Enforcer) EnforceOnBody(ctx context.Context, wsID string, body []byte) ([]byte, EnforcementResult, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, EnforcementResult{}, fmt.Errorf("budget: parse body: %w", err)
	}

	policy := e.GetPolicy(ctx, wsID)

	requestedMax := 0
	if v, ok := m["max_tokens"]; ok {
		switch t := v.(type) {
		case float64:
			requestedMax = int(t)
		case int:
			requestedMax = t
		}
	}

	result := EnforcementResult{
		Original: BudgetPolicy{MaxOutputTokens: requestedMax},
		Applied:  BudgetPolicy{MaxOutputTokens: requestedMax},
	}

	// Unlimited policy: nothing to do.
	if policy.MaxOutputTokens <= 0 {
		return body, result, nil
	}

	switch {
	case requestedMax == 0:
		m["max_tokens"] = policy.MaxOutputTokens
		result.Applied.MaxOutputTokens = policy.MaxOutputTokens
		result.Rewritten = true
		result.Reason = "max_tokens injected from workspace policy"
	case requestedMax > policy.MaxOutputTokens:
		result.Reason = fmt.Sprintf("max_tokens reduced from %d to %d by workspace policy",
			requestedMax, policy.MaxOutputTokens)
		m["max_tokens"] = policy.MaxOutputTokens
		result.Applied.MaxOutputTokens = policy.MaxOutputTokens
		result.Rewritten = true
	default:
		// At or below limit — caller's value stands.
		return body, result, nil
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body, result, fmt.Errorf("budget: marshal body: %w", err)
	}
	return out, result, nil
}

// EstimateInputTokens sums `len(content)/4` across all messages (and the
// top-level `system` field for Anthropic). Same len/4 heuristic the rest
// of the pipeline uses for cost / routing decisions; not exact, but
// stable across components.
func (e *Enforcer) EstimateInputTokens(body []byte) int {
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}
	total := 0
	if len(req.System) > 0 {
		var s string
		if err := json.Unmarshal(req.System, &s); err == nil {
			total += len(s) / 4
		}
	}
	for _, m := range req.Messages {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			total += len(s) / 4
		}
	}
	return total
}
