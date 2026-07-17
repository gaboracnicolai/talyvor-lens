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

// PROPERTY 2 (handler) — the producer attributing its own output → 200; the attribution is scoped to
// the caller's workspace + the path output_id, and the body reports recorded:true.
func TestAttribution_Handler_Producer_200(t *testing.T) {
	rec := &fakeAttrRecorder{owned: true, recorded: true}
	w := serveAttr(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, rec, validAttrBody)
	if w.Code != http.StatusOK {
		t.Fatalf("producer: status=%d, want 200", w.Code)
	}
	if rec.got.WorkspaceID != "ws-A" || rec.got.OutputID != "oid-1" || rec.got.TargetKind != "pr" || rec.got.TargetRef == "" {
		t.Errorf("attribution must be scoped to caller+path; got %+v", rec.got)
	}
	if !strings.Contains(w.Body.String(), `"recorded":true`) {
		t.Errorf("body must report recorded:true; got %s", w.Body.String())
	}
}

// PROPERTY 3 (handler) — an idempotent re-post (store: owned, recorded:false, not conflict) → 200 recorded:false.
func TestAttribution_Handler_Idempotent_200(t *testing.T) {
	rec := &fakeAttrRecorder{owned: true, recorded: false, conflict: false}
	w := serveAttr(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, rec, validAttrBody)
	if w.Code != http.StatusOK {
		t.Fatalf("idempotent: status=%d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"recorded":false`) {
		t.Errorf("body must report recorded:false; got %s", w.Body.String())
	}
}

// PROPERTY 4 (handler) — the store signals a conflicting re-attribution → 409.
func TestAttribution_Handler_Conflict_409(t *testing.T) {
	rec := &fakeAttrRecorder{owned: true, recorded: false, conflict: true}
	w := serveAttr(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, rec, validAttrBody)
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict: status=%d, want 409", w.Code)
	}
}
