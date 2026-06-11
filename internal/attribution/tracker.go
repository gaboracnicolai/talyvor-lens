package attribution

import (
	"net/http"
	"time"
)

// Tracker extracts Git attribution from request headers. Its branch_spend
// write + read methods were retired in #157: the proxy used to double-write
// every attributed request to BOTH branch_spend (here) AND request_attribution
// (Store, context.go) — and since #158 the workspace-scoped reads source from
// request_attribution, leaving branch_spend with no reader. The Tracker now only
// parses attribution. The branch_spend TABLE is left orphaned (no drop
// migration; drop later once confirmed empty in deploys — the ab_tests
// precedent, #154).
type Tracker struct{}

type Attribution struct {
	Branch     string
	PRNumber   string
	CommitSHA  string
	Team       string
	Feature    string
	Repository string
}

// BranchSpend is the aggregated per-branch response shape. It lives on because
// the workspace-scoped reads (Store.GetBranchSpendForWorkspace /
// GetTopBranchesForWorkspace in context.go, #158) return it — sourced from
// request_attribution, not branch_spend.
type BranchSpend struct {
	Branch            string    `json:"branch"`
	PRNumber          string    `json:"pr_number"`
	Repository        string    `json:"repository"`
	TotalCostUSD      float64   `json:"total_cost_usd"`
	TotalInputTokens  int       `json:"total_input_tokens"`
	TotalOutputTokens int       `json:"total_output_tokens"`
	RequestCount      int       `json:"request_count"`
	FirstSeenAt       time.Time `json:"first_seen_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
}

// New builds a Tracker. It no longer needs a DB pool — branch_spend retired in
// #157; the live attribution writes flow through Store → request_attribution.
func New() *Tracker { return &Tracker{} }

func (t *Tracker) ExtractAttribution(r *http.Request) Attribution {
	return Attribution{
		Branch:     r.Header.Get("X-Talyvor-Branch"),
		PRNumber:   r.Header.Get("X-Talyvor-PR"),
		CommitSHA:  r.Header.Get("X-Talyvor-Commit"),
		Team:       r.Header.Get("X-Talyvor-Team"),
		Feature:    r.Header.Get("X-Talyvor-Feature"),
		Repository: r.Header.Get("X-Talyvor-Repository"),
	}
}
