package dashboard

import (
	"net/http"
	"strings"
)

// Handler serves the single-page Talyvor Lens dashboard. The HTML is
// rendered once at construction time with the version baked in so each
// request is just a memcpy.
type Handler struct {
	version    string
	html       []byte
	tokenPages *tokenPages
}

func New(version string) *Handler {
	rendered := strings.ReplaceAll(dashboardHTML, "{{VERSION}}", version)
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
