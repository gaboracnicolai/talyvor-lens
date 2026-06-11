package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// dispatchTokenPage runs one of the three new dashboard
// handlers against a freshly-built Handler. Returns the
// response recorder.
func dispatchTokenPage(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := New("99.0.0", true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	switch path {
	case "/dashboard/tokens":
		h.ServeTokens(rec, req)
	case "/dashboard/nodes":
		h.ServeNodes(rec, req)
	case "/dashboard/economy":
		h.ServeEconomy(rec, req)
	default:
		t.Fatalf("unknown token path %q", path)
	}
	return rec
}

func TestServeTokens_Returns200(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/tokens")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected text/html, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestServeTokens_ContainsBalanceSection(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/tokens")
	body := rec.Body.String()
	for _, want := range []string{
		"LENS Token Balance",
		"Current Balance",
		"Lifetime Earned",
		"id=\"balance-current\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/tokens body", want)
		}
	}
}

func TestServeTokens_ContainsMiningSection(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/tokens")
	body := rec.Body.String()
	for _, want := range []string{
		"Mining Activity",
		"m-cache", "m-compute", "m-embedding", "m-annotation", "m-pattern",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/tokens body", want)
		}
	}
}

func TestServeTokens_ContainsStakingForm(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/tokens")
	body := rec.Body.String()
	for _, want := range []string{
		"Staking",
		"stake-amount", "stake-days",
		"30 days (5% APY)", "90 days (12% APY)", "180 days (20% APY)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/tokens body", want)
		}
	}
}

func TestServeTokens_ContainsMarketplaceForm(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/tokens")
	body := rec.Body.String()
	for _, want := range []string{
		"Marketplace",
		"List LENS for sale",
		"list-amount", "list-price",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/tokens body", want)
		}
	}
}

func TestServeNodes_Returns200AndContainsTables(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/nodes")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Inference Nodes",
		"Embedding Nodes",
		"inference-body", "embedding-body",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/nodes body", want)
		}
	}
}

func TestServeEconomy_Returns200(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/economy")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Global LENS Economy",
		"Total Supply", "Circulating", "Burned", "Staked",
		"Active Marketplace Listings",
		"/v1/economy/stats",
		"/v1/tokens/rates",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/economy body", want)
		}
	}
}

func TestServeEconomy_ContainsConversionSection(t *testing.T) {
	rec := dispatchTokenPage(t, "/dashboard/economy")
	body := rec.Body.String()
	for _, want := range []string{
		"LENS → LXC Conversion",
		"1 LXC = $0.10",
		"Conversion Rate",
		"Fair Rate",
		"Backing / LENS",
		"Rate History",
		"/v1/economy/conversion-rate",
		"/v1/economy/conversion-rate/history",
		"conv-rate", "conv-fair", "conv-backing",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in /dashboard/economy conversion section", want)
		}
	}
}

func TestMainDashboard_ContainsEconomyWidget(t *testing.T) {
	h := New("99.0.0", true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"LENS Economy",
		"lens-widget", "lens-supply",
		"/v1/economy/stats",
		"/dashboard/tokens",
		"/dashboard/nodes",
		"/dashboard/economy",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in main dashboard body", want)
		}
	}
}

func TestTokenPages_ShareVersion(t *testing.T) {
	h := New("0.42.0", true)
	rec := httptest.NewRecorder()
	h.ServeTokens(rec, httptest.NewRequest(http.MethodGet, "/dashboard/tokens", nil))
	if !strings.Contains(rec.Body.String(), "0.42.0") {
		t.Fatal("expected version to appear in tokens page footer")
	}
}
