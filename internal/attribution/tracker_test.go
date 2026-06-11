package attribution

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Tracker's branch_spend Record/GetBranchSpend/GetTopBranches tests were
// removed in #157 with those methods (the double-write had no reader since
// #158). ExtractAttribution — still live (it drives the X-Talyvor-Branch echo
// and the request_attribution write's attribution) — keeps its coverage.

func TestExtractAttribution_AllHeaders(t *testing.T) {
	tr := New()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", nil)
	req.Header.Set("X-Talyvor-Branch", "feature/llm-cache")
	req.Header.Set("X-Talyvor-PR", "1234")
	req.Header.Set("X-Talyvor-Commit", "deadbeef")
	req.Header.Set("X-Talyvor-Team", "platform")
	req.Header.Set("X-Talyvor-Feature", "search")
	req.Header.Set("X-Talyvor-Repository", "acme/lens")

	got := tr.ExtractAttribution(req)

	want := Attribution{
		Branch:     "feature/llm-cache",
		PRNumber:   "1234",
		CommitSHA:  "deadbeef",
		Team:       "platform",
		Feature:    "search",
		Repository: "acme/lens",
	}
	if got != want {
		t.Errorf("ExtractAttribution = %+v, want %+v", got, want)
	}
}

func TestExtractAttribution_NoHeaders(t *testing.T) {
	tr := New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	got := tr.ExtractAttribution(req)
	if got != (Attribution{}) {
		t.Errorf("ExtractAttribution = %+v, want zero value", got)
	}
}
