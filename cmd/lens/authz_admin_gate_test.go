package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
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

type fakePatternAdder struct {
	added   bool
	pattern string
}

func (f *fakePatternAdder) AddPattern(p string) error { f.added, f.pattern = true, p; return nil }

func servePost(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := chi.NewRouter()
	r.Method(http.MethodPost, "/v1/api/injection/patterns", h)
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/api/injection/patterns", strings.NewReader(body)))
	return rec
}

// TestRequireAdmin_InjectionPattern_GateStopsMutation — the 7th class member.
// UNGATED, a non-admin tenant injects a regex into the PROCESS-WIDE detector
// (the #153 leak + a ReDoS vector); GATED, refused 401 and nothing is added; an
// admin passes.
func TestRequireAdmin_InjectionPattern_GateStopsMutation(t *testing.T) {
	tenant := fakeAuthn{ctx: &auth.AuthContext{IsAdmin: false, WorkspaceID: "wsA"}}
	const body = `{"pattern":"evil.*"}`

	// Precondition — the handler DOES mutate the global ruleset when reached.
	ungated := &fakePatternAdder{}
	rec := servePost(newInjectionPatternAddHandler(ungated), body)
	if !ungated.added || rec.Code != http.StatusCreated {
		t.Fatalf("ungated: added=%v code=%d, want true/201 (handler must mutate when reached)", ungated.added, rec.Code)
	}

	// GATED + non-admin: refused, pattern NOT added.
	gated := &fakePatternAdder{}
	rec = servePost(requireAdmin(tenant, http.HandlerFunc(newInjectionPatternAddHandler(gated))), body)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("gated non-admin: status = %d, want 401", rec.Code)
	}
	if gated.added {
		t.Error("global injection ruleset NOT protected: gated non-admin still added a pattern")
	}

	// GATED + admin: passes, pattern added.
	adminAdd := &fakePatternAdder{}
	rec = servePost(requireAdmin(fakeAuthn{ctx: &auth.AuthContext{IsAdmin: true}}, http.HandlerFunc(newInjectionPatternAddHandler(adminAdd))), body)
	if rec.Code != http.StatusCreated || !adminAdd.added {
		t.Errorf("gated admin: status=%d added=%v, want 201/true", rec.Code, adminAdd.added)
	}
}
