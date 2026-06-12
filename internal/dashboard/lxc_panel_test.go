package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

// renderDashboardEcon renders the dashboard with an explicit economy flag (the
// shared renderDashboard helper hardcodes economy=on).
func renderDashboardEcon(t *testing.T, economyEnabled bool) string {
	t.Helper()
	w := httptest.NewRecorder()
	New("1.0.0", economyEnabled).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	return w.Body.String()
}

// TestLXCBalancePanel_PresentAndWired — the fiat LXC credit-balance panel exists,
// is wired to the read-only balance endpoint (GetLXCSnapshot), and renders the
// snapshot figures (balance, USD-at-peg, lifetime purchased/spent). Mirrors the
// roi-panel precedent (client-side section + apply fn + a tries fetch entry).
func TestLXCBalancePanel_PresentAndWired(t *testing.T) {
	html := renderDashboard(t)
	for _, want := range []string{
		`id="lxc-balance-panel"`,                                            // the section
		`id="lxc-balance-summary"`,                                          // the figure container
		`/v1/workspaces/default/lxc/balance`,                                // the read-only fetch (GetLXCSnapshot)
		`function applyLXCBalance(`,                                         // the renderer
		`s.balance`, `s.usd_value`, `s.lifetime_minted`, `s.lifetime_spent`, // the figures
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q (LXC balance panel not wired)", want)
		}
	}
}

// TestLXCBalancePanel_PegFromConst — the peg shown is read from economy.LXCUSDValue,
// never hardcoded. The caption carries the const-derived peg; the USD figure itself
// is sourced from the snapshot's server-computed usd_value (no client-side peg math).
func TestLXCBalancePanel_PegFromConst(t *testing.T) {
	html := renderDashboard(t)
	peg := strconv.FormatFloat(economy.LXCUSDValue, 'f', 2, 64) // "0.10", derived from the const
	if !strings.Contains(html, "$"+peg+" per LXC") {
		t.Errorf("panel must show the peg derived from economy.LXCUSDValue ($%s per LXC)", peg)
	}
	if strings.Contains(html, "{{LXC_USD_PEG}}") {
		t.Error("the {{LXC_USD_PEG}} template var was not substituted (peg not injected from the const)")
	}
}

// TestLXCBalancePanel_FiatNotEconWrapped — the panel is FIAT: present even with the
// economy master OFF (LENS_ECONOMY_ENABLED=false), like the roi-panel. If it were
// wrapped in <!--{{ECON}}-->…<!--{{/ECON}}--> it would be stripped when off.
func TestLXCBalancePanel_FiatNotEconWrapped(t *testing.T) {
	off := renderDashboardEcon(t, false)
	if !strings.Contains(off, `id="lxc-balance-panel"`) {
		t.Error("LXC balance panel must survive the economy master kill (it is fiat)")
	}
	if strings.Contains(off, "{{ECON}}") {
		t.Error("ECON markers must be stripped when economy off")
	}
}
