package main

import (
	"context"
	"net/http"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/outputverify"
)

// outputVerdictLister is the read seam for the K4 verdict surfaces (Query-only; *outputverify.Reader
// satisfies it). Kept an interface so both gates are testable without a DB.
type outputVerdictLister interface {
	ListAll(ctx context.Context, limit int) ([]outputverify.ListedVerdict, error)
	ListForWorkspace(ctx context.Context, workspaceID string, limit int) ([]outputverify.ListedVerdict, error)
}

// newOutputVerdictsAdminHandler serves ALL workspaces' verdicts — wrapped by requireAdmin at the mount site
// (an admin forensic read). Rows are hashes-only + self-workspace-tagged; no raw content, no counterparty.
func newOutputVerdictsAdminHandler(l outputVerdictLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		vs, err := l.ListAll(req.Context(), 100)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if vs == nil {
			vs = []outputverify.ListedVerdict{}
		}
		writeJSONOK(w, http.StatusOK, vs)
	}
}

// verdictAuthenticator resolves the caller's identity (the authManager satisfies it).
type verdictAuthenticator interface {
	Authenticate(*http.Request) (*auth.AuthContext, error)
}

// newOutputVerdictsWorkspaceHandler serves ONLY the calling workspace's OWN verdicts — a tenant may read
// its own, never another's. It scopes strictly to the authenticated WorkspaceID; a caller with no resolved
// workspace is refused (401). There is no way to name another workspace's verdicts through this surface.
func newOutputVerdictsWorkspaceHandler(authn verdictAuthenticator, l outputVerdictLister) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ac, err := authn.Authenticate(req)
		if err != nil || ac == nil || ac.WorkspaceID == "" {
			writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		vs, err := l.ListForWorkspace(req.Context(), ac.WorkspaceID, 100)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if vs == nil {
			vs = []outputverify.ListedVerdict{}
		}
		writeJSONOK(w, http.StatusOK, vs)
	}
}
