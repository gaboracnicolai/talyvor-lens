package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
)

// fakeResetter records whether Reset was reached — for the no-state-change-past-the-gate proof.
type fakeResetter struct{ called bool }

func (f *fakeResetter) Reset(ctx context.Context, annotatorID, by, note string) (float64, error) {
	f.called = true
	return 0.5, nil
}

// ADMIN-GATE — the reset endpoint × {non-admin, unauthenticated} → 401 with NO state change
// (Reset never reached) and no data leaked. Admin → 200, Reset reached.
func TestReputationReset_AdminGate(t *testing.T) {
	body := func() *strings.Reader { return strings.NewReader(`{"annotator_id":"bad","reason":"x"}`) }
	for _, tc := range []struct {
		name string
		a    fakeAuthenticator
	}{
		{"non-admin", fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: false, UserID: "ws-7"}}},
		{"unauthenticated", fakeAuthenticator{err: http.ErrNoCookie}},
	} {
		store := &fakeResetter{}
		h := requireAdmin(tc.a, http.HandlerFunc(newReputationResetHandler(tc.a, store)))
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/annotation-reputation/reset", body()))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code %d want 401", tc.name, rec.Code)
		}
		if store.called {
			t.Errorf("%s: Reset was reached despite 401 — NO state change may pass the gate", tc.name)
		}
		if strings.Contains(rec.Body.String(), "reputation") {
			t.Errorf("%s: 401 body leaked data: %s", tc.name, rec.Body.String())
		}
	}
	// Sanity: a real admin passes → Reset reached, 200.
	store := &fakeResetter{}
	admin := fakeAuthenticator{ctx: &auth.AuthContext{IsAdmin: true, UserID: "admin1"}}
	h := requireAdmin(admin, http.HandlerFunc(newReputationResetHandler(admin, store)))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/annotation-reputation/reset", body()))
	if rec.Code != http.StatusOK {
		t.Errorf("admin: code %d want 200", rec.Code)
	}
	if !store.called {
		t.Error("admin: Reset should be reached")
	}
}
