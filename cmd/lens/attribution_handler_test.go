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

type fakeAttrRecorder struct {
	owned, recorded, conflict bool
	got                       outputverify.Attribution
	called                    bool
}

func (f *fakeAttrRecorder) RecordAttributionIfOwned(_ context.Context, a outputverify.Attribution) (bool, bool, bool, error) {
	f.called = true
	f.got = a
	return f.owned, f.recorded, f.conflict, nil
}

const validAttrBody = `{"target_kind":"pr","target_ref":"https://github.com/x/y/pull/1"}`

func serveAttr(authn fakeAuthn, rec *fakeAttrRecorder, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Post("/v1/outputs/{output_id}/attribution", newAttributionHandler(authn, rec))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/outputs/oid-1/attribution", strings.NewReader(body)))
	return w
}

// PROPERTY 1 (handler) — CROSS-TENANT: a caller that is not the producer of output_id → 403.
func TestAttribution_Handler_CrossTenant_403(t *testing.T) {
	notOwner := &fakeAttrRecorder{owned: false}
	w := serveAttr(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-B"}}, notOwner, validAttrBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant attribution: status=%d, want 403 (not the producer)", w.Code)
	}
	// The handler DID reach the store (ownership is enforced in the store, not guessed) and the store
	// said owned=false → 403; the caller's workspace + path output_id were passed through faithfully.
	if !notOwner.called || notOwner.got.WorkspaceID != "ws-B" || notOwner.got.OutputID != "oid-1" {
		t.Errorf("handler must pass caller+path to the store; got %+v", notOwner.got)
	}
}
