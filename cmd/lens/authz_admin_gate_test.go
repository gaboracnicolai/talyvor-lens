package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// authz_admin_gate_test.go — #153. The gate (requireAdmin) is the security
// primitive; proven here behaviorally over HTTP. Every one of the six
// global-config write routes is wrapped in it (wiring verified structurally in
// run()); the pool-key delete is exercised end-to-end as the representative
// mutating route.

type fakeAuthn struct {
	ctx *auth.AuthContext
	err error
}

func (f fakeAuthn) Authenticate(*http.Request) (*auth.AuthContext, error) { return f.ctx, f.err }

type spyHandler struct{ called bool }

func (s *spyHandler) ServeHTTP(http.ResponseWriter, *http.Request) { s.called = true }

func TestRequireAdmin_Gate(t *testing.T) {
	cases := []struct {
		name      string
		authn     fakeAuthn
		wantCode  int
		wantInner bool
	}{
		{"admin passes", fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, http.StatusOK, true},
		{"non-admin tenant refused", fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}, http.StatusUnauthorized, false},
		{"missing creds fail closed", fakeAuthn{err: auth.ErrMissingCredentials}, http.StatusUnauthorized, false},
		{"nil context fail closed", fakeAuthn{ctx: nil, err: nil}, http.StatusUnauthorized, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := &spyHandler{}
			rec := httptest.NewRecorder()
			requireAdmin(c.authn, spy)(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
			if rec.Code != c.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, c.wantCode)
			}
			if spy.called != c.wantInner {
				t.Errorf("inner handler called = %v, want %v (must not run when refused)", spy.called, c.wantInner)
			}
		})
	}
}

type fakePoolKey struct{ removed bool }

func (f *fakePoolKey) Remove(string) bool { f.removed = true; return true }

// serveDelete mounts h on a real chi router and fires a DELETE — exercises the
// {keyID} param extraction the real route uses.
func serveDelete(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Method(http.MethodDelete, "/v1/api/keys/pool/{keyID}", h)
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/api/keys/pool/k1", nil))
	return rec
}

// TestRequireAdmin_PoolKeyDelete_GateStopsMutation is the wiring proof for the
// representative route. UNGATED, a non-admin evicts a shared provider key (the
// #153 vulnerability); GATED, the same caller is refused 401 and the key
// survives; an admin passes and the key is removed.
func TestRequireAdmin_PoolKeyDelete_GateStopsMutation(t *testing.T) {
	tenant := fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}

	// Precondition — the handler DOES mutate when reached (so the gate's refusal
	// below is meaningful, not a no-op handler).
	ungated := &fakePoolKey{}
	rec := serveDelete(newPoolKeyDeleteHandler(ungated))
	if !ungated.removed || rec.Code != http.StatusOK {
		t.Fatalf("ungated: removed=%v code=%d, want true/200 (handler must mutate when reached)", ungated.removed, rec.Code)
	}

	// GATED + non-admin: refused, key NOT removed.
	gatedPool := &fakePoolKey{}
	rec = serveDelete(requireAdmin(tenant, http.HandlerFunc(newPoolKeyDeleteHandler(gatedPool))))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("gated non-admin: status = %d, want 401", rec.Code)
	}
	if gatedPool.removed {
		t.Error("DoS NOT closed: gated non-admin still evicted the pool key")
	}

	// GATED + admin: passes, key removed.
	adminPool := &fakePoolKey{}
	rec = serveDelete(requireAdmin(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, http.HandlerFunc(newPoolKeyDeleteHandler(adminPool))))
	if rec.Code != http.StatusOK || !adminPool.removed {
		t.Errorf("gated admin: status=%d removed=%v, want 200/true", rec.Code, adminPool.removed)
	}
}
