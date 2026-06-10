package main

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
)

// authz_admin_handlers.go — the #153 admin gate. The six global-config WRITE
// routes mutate PROCESS-WIDE state (local-routing endpoints, the provider key
// pool, fallback chains) and were reachable by any authenticated tenant; they
// are now wrapped in requireAdmin. Extracted to package level so the gate is
// provable over HTTP (the established testability pattern).

// adminAuthenticator is the subset of *auth.Manager requireAdmin needs.
type adminAuthenticator interface {
	Authenticate(r *http.Request) (*auth.AuthContext, error)
}

// requireAdmin gates next so only the global admin key reaches it. It FAILS
// CLOSED: a missing, invalid, or non-admin credential (or a nil context) → 401.
// Admin is the AuthContext.IsAdmin carrier resolved by auth.Manager.Authenticate
// — the same source every authz fix has used since #147, never a header or a
// config-string compare.
func requireAdmin(am adminAuthenticator, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actx, err := am.Authenticate(r)
		if err != nil || actx == nil || !actx.IsAdmin {
			writeJSONErr(w, http.StatusUnauthorized, "admin credentials required")
			return
		}
		next.ServeHTTP(w, r)
	}
}

// poolKeyRemover is the slice of *keypool.Pool the delete handler needs.
type poolKeyRemover interface {
	Remove(keyID string) bool
}

// newPoolKeyDeleteHandler — DELETE /v1/api/keys/pool/{keyID}. Extracted so the
// admin gate's behavior on a real mutating handler is provable (the #153 wiring
// proof): without the gate a tenant evicts a shared provider key.
func newPoolKeyDeleteHandler(pool poolKeyRemover) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		id := chi.URLParam(req, "keyID")
		if !pool.Remove(id) {
			writeJSONErr(w, http.StatusNotFound, "key not found")
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// patternAdder is the slice of *injection.Detector the pattern handler needs.
type patternAdder interface {
	AddPattern(pattern string) error
}

// newInjectionPatternAddHandler — POST /v1/api/injection/patterns. The seventh
// member of the #153 class (folded in): AddPattern mutates the single PROCESS-
// WIDE injection detector and accepts arbitrary regex (a ReDoS vector), so it
// is admin-only like the other global-config writes.
func newInjectionPatternAddHandler(adder patternAdder) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Pattern string `json:"pattern"`
		}
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if err := adder.AddPattern(in.Pattern); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, map[string]bool{"ok": true})
	}
}
