package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/platformreport"
)

// B32.44 — the export route refuses a request it cannot make before it reads anything. Its file, sha256 and recorded
// run are proven on Postgres in internal/platformreport; a real-PG test here would migrate a whole schema in a package
// whose tests already run close to CI's 300s limit.
func TestPlatformReportRoute_RefusesAnUnknownFormatOrAMissingOperator(t *testing.T) {
	g := platformreport.New(nil, nil)
	for _, body := range []string{
		`{"year": 2026, "format": "xlsx", "actor": "nicolai"}`,
		`{"year": 2026, "format": "csv"}`,
	} {
		w := httptest.NewRecorder()
		newPlatformReportExportHandler(g).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/admin/platform-reports", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
}
