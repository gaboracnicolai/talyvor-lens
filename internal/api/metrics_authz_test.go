package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// ITEM 5: /metrics (cmd/lens/main.go) is admin-gated ("scrapers must send Authorization: Bearer") but the
// twin /v1/api/metrics/prometheus was registered in MountUnauthenticated — the SAME promhttp handler,
// served with no auth, defeating the gate and leaking economy/internal telemetry. RED: the twin is served
// on the unauthenticated router. GREEN: it is not (moved behind auth); /v1/api/health stays open.
func TestAPI_MetricsPrometheus_NotServedUnauthenticated(t *testing.T) {
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	s := newServerWithPool(t, pool)

	r := chi.NewRouter()
	s.MountUnauthenticated(r) // the bare, no-auth router (as cmd/lens/main.go mounts it)

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/api/metrics/prometheus", nil))
	if rr.Code == http.StatusOK {
		t.Errorf("/v1/api/metrics/prometheus is served UNAUTHENTICATED (status %d) — internal telemetry leak; /metrics is admin-gated, this twin must be too", rr.Code)
	}

	// The health probe MUST remain unauthenticated.
	hr := httptest.NewRecorder()
	r.ServeHTTP(hr, httptest.NewRequest(http.MethodGet, "/v1/api/health", nil))
	if hr.Code == http.StatusNotFound {
		t.Errorf("/v1/api/health should remain unauthenticated, got 404")
	}
}
