package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/platformreport"
	"github.com/talyvor/lens/migrations"
)

// B32.44 — the operator downloads the year's platform-reporting export through the route, and the run it recorded
// carries the downloaded file's sha256.
func TestPlatformReportRoute_DownloadsTheFileAndListsItsRun(t *testing.T) {
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG platform report route test")
	}
	const schema = "cmd_platform_reports_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	g := platformreport.New(pool, nil)

	post := httptest.NewRecorder()
	newPlatformReportExportHandler(g).ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/v1/admin/platform-reports",
		strings.NewReader(`{"year": 2026, "funding": "test", "format": "csv", "actor": "nicolai"}`)))
	if post.Code != http.StatusOK || !strings.HasPrefix(post.Body.String(), "year,funding,currency,workspace_id,") {
		t.Fatalf("POST = %d %q, want the CSV file", post.Code, post.Body.String())
	}
	sum := sha256.Sum256(post.Body.Bytes())
	if got := post.Header().Get("X-Platform-Report-Sha256"); got != hex.EncodeToString(sum[:]) ||
		post.Header().Get("Content-Disposition") != `attachment; filename="platform-report-2026-test.csv"` {
		t.Fatalf("headers %v; want the file's sha256 %x and its name", post.Header(), sum)
	}

	list := httptest.NewRecorder()
	newPlatformReportRunsHandler(g).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/v1/admin/platform-reports?year=2026", nil))
	var body struct {
		Runs []platformreport.Run `json:"runs"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || len(body.Runs) != 1 {
		t.Fatalf("GET = %d %s (%v), want the one run", list.Code, list.Body.String(), err)
	}
	if r := body.Runs[0]; r.SHA256 != hex.EncodeToString(sum[:]) || r.Operator != "nicolai" || r.Format != "csv" || r.Funding != "test" {
		t.Fatalf("the run = %+v, want nicolai's test CSV with the downloaded file's sha256", r)
	}

	bad := httptest.NewRecorder()
	newPlatformReportExportHandler(g).ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/v1/admin/platform-reports",
		strings.NewReader(`{"year": 2026, "format": "xlsx", "actor": "nicolai"}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("an unknown format = %d, want 400", bad.Code)
	}
}
