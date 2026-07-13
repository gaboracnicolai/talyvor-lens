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

type fakeMechRecorder struct {
	owned, recorded bool
	got             outputverify.MechanicalReport
	called          bool
}

func (f *fakeMechRecorder) RecordMechanicalIfOwned(_ context.Context, r outputverify.MechanicalReport) (bool, bool, error) {
	f.called = true
	f.got = r
	return f.owned, f.recorded, nil
}

func TestMechanicalVerdict_Handler(t *testing.T) {
	serve := func(authn fakeAuthn, rec *fakeMechRecorder, body string) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.Post("/v1/output-verdicts/{output_id}/mechanical", newMechanicalVerdictHandler(authn, rec))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/output-verdicts/oid-1/mechanical", strings.NewReader(body)))
		return w
	}
	body := `{"verdict":"compile_failed","exit_code":1,"tool":"go build"}`

	// (a) authed + owned → 200; report scoped to the caller's workspace + the path output_id.
	owned := &fakeMechRecorder{owned: true, recorded: true}
	w := serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, owned, body)
	if w.Code != http.StatusOK {
		t.Fatalf("owned: status=%d, want 200", w.Code)
	}
	if owned.got.WorkspaceID != "ws-A" || owned.got.OutputID != "oid-1" || owned.got.Verdict != "compile_failed" {
		t.Errorf("report must be scoped to caller+path; got %+v", owned.got)
	}

	// (b) not the producer → 403.
	notOwned := &fakeMechRecorder{owned: false}
	w = serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-B"}}, notOwned, body)
	if w.Code != http.StatusForbidden {
		t.Errorf("not-owned: status=%d, want 403", w.Code)
	}

	// (c) unauthenticated (no workspace) → 401, recorder never reached.
	unauth := &fakeMechRecorder{}
	w = serve(fakeAuthn{ctx: &auth.AuthContext{}}, unauth, body)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauth: status=%d, want 401", w.Code)
	}
	if unauth.called {
		t.Error("unauth: the recorder must not be reached")
	}

	// (d) invalid verdict enum → 400.
	inv := &fakeMechRecorder{owned: true}
	w = serve(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-A"}}, inv, `{"verdict":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid verdict: status=%d, want 400", w.Code)
	}
	if inv.called {
		t.Error("invalid verdict: the recorder must not be reached")
	}
}
