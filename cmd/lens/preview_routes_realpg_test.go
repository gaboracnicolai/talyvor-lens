package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/distill"
)

// inProcessConverter converts in-process — the production mount uses the subprocess isolator, whose
// worker binary a unit test does not have. The handler under test is the same.
type inProcessConverter struct{}

func (inProcessConverter) Convert(ctx context.Context, in []byte, f distill.Format) (distill.Result, error) {
	return distill.DistillAs(ctx, in, f)
}

// previewLedgers applies the real migrations that create token_events and both charge ledgers
// (lxc_ledger, subscription_allowance) into a scratch schema, and returns a row counter over them.
func previewLedgers(t *testing.T) func() int {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("LENS_TEST_DATABASE_URL not set — this test must RUN, not skip")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	const schema = "b114_previews"
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, f := range []string{"0001_init.sql", "0027_lxc_credits.sql", "0121_subscription_allowance.sql"} {
		ddl, err := os.ReadFile("../../migrations/" + f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM token_events) + (SELECT count(*) FROM lxc_ledger)
			+ (SELECT count(*) FROM subscription_allowance)`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
}

// previewRouter is the production shape: the authed group's workspace isolation, then mountPreviewRoutes.
// The caller is authenticated as workspace ws_a.
func previewRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws_a"})))
		})
	})
	r.Group(func(authed chi.Router) {
		authed.Use(workspaceIsolationMiddleware)
		mountPreviewRoutes(authed, inProcessConverter{})
	})
	return r
}

func previewPost(t *testing.T, h http.Handler, path, contentType string, body []byte) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, got
}

// B11.4 — both previews answer through the real handlers, write no charge and no token_events row, and
// refuse a caller whose credential is not bound to the path's workspace.
func TestTryItPreviews_AnswerWithoutChargingAndRefuseAForeignWorkspace(t *testing.T) {
	rows := previewLedgers(t)
	h := previewRouter()
	before := rows()

	rowsJSON := `{"users":[{"id":1,"name":"ada","role":"admin"},{"id":2,"name":"bob","role":"dev"},{"id":3,"name":"cy","role":"dev"},{"id":4,"name":"di","role":"ops"}]}`
	body, _ := json.Marshal(map[string]string{"content": rowsJSON, "kind": "json", "model": "gpt-4o"})
	code, got := previewPost(t, h, "/v1/workspaces/ws_a/tare/preview", "application/json", body)
	if code != http.StatusOK {
		t.Fatalf("tare preview: %d %v", code, got)
	}
	if got["refused"] != false || got["kind"] != "json" || len(got["reduced"].(string)) >= len(rowsJSON) {
		t.Errorf("tare preview did not reduce a same-shaped JSON array: %v", got)
	}
	if saved, _ := got["tokens_saved_estimated"].(float64); saved <= 0 {
		t.Errorf("tokens_saved_estimated = %v, want > 0", got["tokens_saved_estimated"])
	}
	if _, ok := got["saving_usd_estimated"]; !ok {
		t.Error("a priced model was named and no estimated saving came back")
	}

	prose, _ := json.Marshal(map[string]string{"content": "Please summarise the attached meeting notes.", "kind": "prose"})
	if code, got := previewPost(t, h, "/v1/workspaces/ws_a/tare/preview", "application/json", prose); code != http.StatusOK || got["refused"] != true {
		t.Errorf("prose must come back refused and unchanged: %d %v", code, got)
	}

	code, got = previewPost(t, h, "/v1/workspaces/ws_a/distill/preview", "text/html",
		[]byte("<html><body><h1>Quarterly plan</h1><p>Ship the preview.</p></body></html>"))
	if code != http.StatusOK || !strings.Contains(got["markdown"].(string), "Quarterly plan") {
		t.Errorf("conversion preview: %d %v", code, got)
	}

	for _, path := range []string{"/v1/workspaces/ws_b/tare/preview", "/v1/workspaces/ws_b/distill/preview"} {
		if code, _ := previewPost(t, h, path, "application/json", body); code != http.StatusForbidden {
			t.Errorf("%s from a ws_a credential: %d, want 403", path, code)
		}
	}

	if after := rows(); after != before {
		t.Errorf("previews wrote %d charge/token_events rows, want 0", after-before)
	}
}
