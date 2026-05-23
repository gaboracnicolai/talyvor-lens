package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboard_Returns200(t *testing.T) {
	h := New("1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestDashboard_ContentTypeHTML(t *testing.T) {
	h := New("1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	got := w.Header().Get("Content-Type")
	if !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html prefix", got)
	}
}

func TestDashboard_BodyContainsBrand(t *testing.T) {
	h := New("1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "TALYVOR LENS") {
		t.Error("dashboard body does not contain brand string 'TALYVOR LENS'")
	}
}

func TestDashboard_RootRedirectsToDashboard(t *testing.T) {
	h := New("1.2.3")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.RedirectRoot(w, req)

	if w.Code != http.StatusFound {
		t.Errorf("status = %d, want 302 Found", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", loc)
	}
}

func TestDashboard_BodyContainsVersion(t *testing.T) {
	const version = "7.8.9"
	h := New(version)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), version) {
		t.Errorf("dashboard body does not contain version %q", version)
	}
}
