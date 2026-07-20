package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/outputverify"
)

type fakeAttributionReader struct {
	byOutput map[string][]outputverify.AttributionRecord // key "ws|oid"
	list     []outputverify.AttributionRecord
	gotWS    string
}

func (f *fakeAttributionReader) GetByOutput(_ context.Context, ws, oid string) ([]outputverify.AttributionRecord, error) {
	return f.byOutput[ws+"|"+oid], nil
}
func (f *fakeAttributionReader) ListByWorkspace(_ context.Context, ws string, _ int) ([]outputverify.AttributionRecord, error) {
	f.gotWS = ws
	return f.list, nil
}

// GET /v1/outputs/{output_id}/attribution: own output with attributions → 200 array; a foreign output (or an
// owned output with none) → 404 (no cross-tenant oracle); no credential → 401.
func TestAttributionByOutput_Handler(t *testing.T) {
	reader := &fakeAttributionReader{byOutput: map[string][]outputverify.AttributionRecord{
		"wsA|oid-A": {{OutputID: "oid-A", WorkspaceID: "wsA", TargetKind: "pr", TargetRef: "https://x/pr/1"}},
	}}
	serve := func(ac *auth.AuthContext, oid string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Get("/v1/outputs/{output_id}/attribution", newAttributionByOutputHandler(fakeAuthn{ctx: ac}, reader))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/outputs/"+oid+"/attribution", nil))
		return rec
	}
	rec := serve(&auth.AuthContext{WorkspaceID: "wsA"}, "oid-A")
	if rec.Code != http.StatusOK {
		t.Fatalf("own output: status=%d, want 200", rec.Code)
	}
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") || !strings.Contains(rec.Body.String(), `"pr"`) {
		t.Errorf("must be a JSON array with the attribution; body=%s", rec.Body.String())
	}
	// foreign / empty → 404, not 403
	rec = serve(&auth.AuthContext{WorkspaceID: "wsA"}, "oid-foreign")
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign output: status=%d, want 404 (no oracle)", rec.Code)
	}
	// no credential → 401
	rec = serve(&auth.AuthContext{}, "oid-A")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no credential: status=%d, want 401", rec.Code)
	}
}

// GET /v1/attributions: self-scoped array of the caller's own attributions.
func TestAttributionList_Handler(t *testing.T) {
	reader := &fakeAttributionReader{list: []outputverify.AttributionRecord{{OutputID: "o1", WorkspaceID: "wsA"}}}
	h := newAttributionListHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "wsA"}}, reader)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/attributions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if reader.gotWS != "wsA" {
		t.Errorf("must scope to caller's workspace; got %q", reader.gotWS)
	}
	if !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("attribution list must be a JSON array; body=%s", rec.Body.String())
	}
}
