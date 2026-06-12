package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func renderDashboard(t *testing.T) string {
	t.Helper()
	h := New("test-version", true)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	return w.Body.String()
}

// The DISTILL panel must render: its section, honest labels (saved vs cost vs
// net vs cache), the apply function, and its fetch-table registration.
func TestDashboard_ContainsDistillPanel(t *testing.T) {
	body := renderDashboard(t)
	for _, want := range []string{
		`id="distill-panel"`,
		"Tokens Saved",
		"Vision-OCR Cost",
		"Net Token Impact",
		"Cache Hit Rate",
		"function applyDistill",
		"/v1/api/distill/summary",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

// Anti-#28-gap guard: the one data-derived string the panel surfaces (the
// basis note) MUST be escaped — never inlined raw. (#28's gap was an
// un-escaped data string in applyLocal; this panel must not repeat it.)
func TestDashboard_DistillPanelEscapesBasis(t *testing.T) {
	body := renderDashboard(t)
	if !strings.Contains(body, "escapeHTML(d.basis") {
		t.Error("DISTILL panel must wrap the data-derived basis string in escapeHTML (don't repeat #28's gap)")
	}
}

// The numeric metrics must be rendered through the numeric formatters
// (textContent / fmtInt / fmtPct) — never raw string interpolation — so the
// panel has no string XSS surface for its counters.
func TestDashboard_DistillPanelNumbersAreNumeric(t *testing.T) {
	body := renderDashboard(t)
	for _, want := range []string{
		"fmtInt(d.tokens_saved",
		"fmtPct(d.cache_hit_rate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DISTILL panel should format %q numerically", want)
		}
	}
}
