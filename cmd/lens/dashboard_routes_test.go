package main

// The browser-surface gate at the ROUTE layer, and the one guarantee that holds
// on BOTH sides of it: the root always answers.
//
// WHY THIS SURFACE SHRANK. Lens used to serve a 41-panel dashboard here. Ten of
// its thirteen data panels called AUTHENTICATED endpoints with a bare
// `fetch(path)` that attached no credential, and the page had no way to accept
// one — so on the deployed host every one of them answered
// 401 {"error":"API key required"} and rendered a permanent em-dash. It could
// not show a visitor their own data, which was the entire case for Lens shipping
// a UI of its own. The fix is not a key box (that would put a long-lived API key
// in browser localStorage ON THE API HOSTNAME — precisely the exposure the
// suite's BFF exists to prevent by holding the key server-side behind a session);
// the fix is to stop pretending. What remains states what the service is and
// where its UI lives, and shows only what it can prove without a credential.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/dashboard"
)

// wire builds the browser surface exactly as main.go does, for a given flag.
func wire(on bool) chi.Router {
	r := chi.NewRouter()
	h := dashboard.New("0.1.0")
	dash := dashReg{on: on}
	dash.get(r, "/dashboard", h.ServeHTTP)
	r.Get("/", h.ServeHTTP) // ALWAYS registered — see TestRootAlwaysAnswers
	return r
}

func hitPath(r chi.Router, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestDashboardFlagOff_RoutesAbsent: default-off must mean the routes are NEVER
// REGISTERED — a chi-native 404, indistinguishable from a path that was never
// built — not a handler that answers "disabled". Same shape as billReg.
func TestDashboardFlagOff_RoutesAbsent(t *testing.T) {
	r := wire(false)
	for _, p := range []string{"/dashboard", "/dashboard/tokens", "/dashboard/nodes", "/dashboard/oracle", "/dashboard/economy"} {
		if got := hitPath(r, p).Code; got != http.StatusNotFound {
			t.Errorf("%s with the flag OFF: got %d, want 404 (route must not be registered)", p, got)
		}
	}
}

// TestDashboardFlagOn_RoutePresent: turning it on registers the page.
func TestDashboardFlagOn_RoutePresent(t *testing.T) {
	rec := hitPath(wire(true), "/dashboard")
	if rec.Code != http.StatusOK {
		t.Fatalf("/dashboard with the flag ON: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Talyvor Lens") {
		t.Error("the served page must name the service")
	}
}

// TestRootAlwaysAnswers is the promise that outlives the flag. Someone who types
// the API hostname into a browser must never be met with nothing — not a bare
// 404 (honest but useless) and not a redirect to app.talyvor.com (which assumes
// a suite deployment a self-hoster may not have). The root is a static page with
// no credential, no database read and no per-workspace data, so there is nothing
// about it worth gating; the flag governs the /dashboard path, not this.
func TestRootAlwaysAnswers(t *testing.T) {
	for _, on := range []bool{true, false} {
		rec := hitPath(wire(on), "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / with flag=%v: got %d, want 200 — the root must never leave a visitor with nothing", on, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Talyvor Lens") {
			t.Errorf("flag=%v: the root must name the service", on)
		}
		if !strings.Contains(body, "app.talyvor.com") {
			t.Errorf("flag=%v: the root must point at where the dashboard actually is", on)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("flag=%v: Content-Type = %q, want text/html", on, ct)
		}
	}
}

// TestRootDoesNotRedirect: the old root 302'd to /dashboard. With the dashboard
// default-off that would send every visitor to a 404 — a redirect into nothing
// is worse than either endpoint alone.
func TestRootDoesNotRedirect(t *testing.T) {
	for _, on := range []bool{true, false} {
		rec := hitPath(wire(on), "/")
		if rec.Code >= 300 && rec.Code < 400 {
			t.Errorf("flag=%v: GET / returned a %d redirect to %q — the root must serve, not bounce",
				on, rec.Code, rec.Header().Get("Location"))
		}
	}
}
