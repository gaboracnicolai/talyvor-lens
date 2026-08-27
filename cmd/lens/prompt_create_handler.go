package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/lens/internal/prompts"
)

// prompt_create_handler.go — POST /v1/prompts, and the workspace it may write to.
//
// ⚠ WHAT THIS ROUTE DID BEFORE. It decoded a prompts.Prompt from the request body
// — `workspace_id` and all — and handed it straight to promptManager.Create. No
// credential was consulted, so the workspace a prompt was created IN was whatever
// the caller typed. Every sibling route on this resource had already been fixed:
//
//	PUT    /v1/prompts/{name}           effectiveWorkspaceID   (#146)
//	POST   /v1/prompts/{name}/rollback  effectiveWorkspaceID   (#146)
//	GET    /v1/prompts                  applyPhase2WSID
//	GET    /v1/prompts/{name}           applyPhase2WSID
//	GET    /v1/prompts/{name}/history   applyPhase2WSID
//	GET    /v1/prompts/{name}/diff      applyPhase2WSID
//	POST   /v1/prompts                  — nothing —
//
// The #146 sweep closed mutate and read on this resource and missed CREATE.
//
// ⚠ AND IT REACHED THE SERVING PATH, which is why this is not merely an untidy
// row. proxy.go calls promptManager.Resolve(ctx, body, wsID) on every request,
// swapping any `lens:prompt:<name>` system message for the stored content of that
// name IN THE CALLER'S OWN WORKSPACE. prompts.Manager.Get reads an in-memory cache
// keyed (name, workspaceID) BEFORE it reads Postgres, and Create writes that cache
// entry unconditionally — so a create naming somebody else's workspace replaced
// what their next request would have had substituted into it, in-process and
// immediately, whatever the database row said.
//
// The fix is not a new policy: it is the rule #146 already decided, applied to the
// route the sweep missed. A non-admin is forced to its own workspace; the global
// admin still honours the body, which is what effectiveWorkspaceID means
// everywhere else in this file.

// promptCreator is the slice of *prompts.Manager this route needs.
type promptCreator interface {
	Create(ctx context.Context, p prompts.Prompt) (*prompts.Prompt, error)
}

func newPromptCreateHandler(mgr promptCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in prompts.Prompt
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		// Authz (#146, applied here by W6.13): a non-admin may create only in its
		// OWN workspace; the global admin honours the body, and an empty value
		// keeps Create's existing "default" behaviour for it.
		eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		in.WorkspaceID = eff
		created, err := mgr.Create(req.Context(), in)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusCreated, created)
	}
}
