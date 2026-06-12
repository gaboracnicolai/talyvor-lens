package dashboard

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/economy"
)

// econBlockRE matches an <!--{{ECON}}-->…<!--{{/ECON}}--> economy-only fragment.
var econBlockRE = regexp.MustCompile(`(?s)<!--\{\{ECON\}\}-->.*?<!--\{\{/ECON\}\}-->`)

// Handler serves the single-page Talyvor Lens dashboard. The HTML is
// rendered once at construction time with the version baked in so each
// request is just a memcpy.
type Handler struct {
	version    string
	html       []byte
	tokenPages *tokenPages
}

// New renders the dashboard. When economyEnabled is false (U3 master switch off),
// economy-only fragments wrapped in <!--{{ECON}}-->…<!--{{/ECON}}--> are stripped
// (the economy nav links); the data-driven economy panels self-hide anyway, since
// their /v1/api/* and /v1/economy/* fetches are unregistered → 404. When on, only
// the marker comments are removed and the content stays.
func New(version string, economyEnabled bool) *Handler {
	rendered := strings.ReplaceAll(dashboardHTML, "{{VERSION}}", version)
	// The fiat LXC peg (#182) — read from the const, never hardcoded in the UI.
	rendered = strings.ReplaceAll(rendered, "{{LXC_USD_PEG}}",
		strconv.FormatFloat(economy.LXCUSDValue, 'f', 2, 64))
	if economyEnabled {
		rendered = strings.NewReplacer("<!--{{ECON}}-->", "", "<!--{{/ECON}}-->", "").Replace(rendered)
	} else {
		rendered = econBlockRE.ReplaceAllString(rendered, "")
	}
	return &Handler{
		version: version,
		html:    []byte(rendered),
	}
}

// ServeHTTP serves the dashboard page itself. Cacheability is left to
// callers — the page is a static template, but the data it shows comes
// from XHRs that hit /v1/api/* every 30 seconds.
func (h *Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.html)
}

// RedirectRoot is the helper for `GET /` — sends a 302 to the dashboard
// so the root URL acts as the human entry point.
func (h *Handler) RedirectRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}
