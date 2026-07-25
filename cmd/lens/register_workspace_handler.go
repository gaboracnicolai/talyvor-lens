package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/lens/internal/workspace"
)

// registrar is the workspace-registration seam (*workspace.Manager satisfies it). Extracted so the
// POST /v1/workspaces handler is testable and can be wrapped in requireAdmin at the route.
type registrar interface {
	RegisterWorkspace(ctx context.Context, ws workspace.Workspace, opts ...workspace.RegisterOption) error
	// CachePoolableConsent reports the consent actually RECORDED, so the response can state the
	// pooling state the workspace ended up in instead of echoing what was asked for.
	CachePoolableConsent(wsID string) (poolable bool, known bool)
}

// registerWorkspaceRequest is the WIRE shape of POST /v1/workspaces. It embeds workspace.Workspace
// so every field stays decoded (and no future field is silently dropped by a hand-copied DTO), and
// SHADOWS one field: cache_poolable.
//
// The shadow is the whole point. workspace.Workspace.CachePoolable is a plain bool, so on the wire an
// omitted field and an explicit `false` both decode to false — indistinguishable, and the
// new-workspace default then overwrote both. A privacy-conscious tenant therefore could not decline
// cross-tenant pooling at creation; it had to discover the default afterwards and call
// SetCachePoolable. As *bool, nil means "said nothing" (take the default) and a non-nil false means
// "declined" (honoured).
//
// Go's encoding/json resolves the collision by depth: this field sits at depth 0 and wins over the
// promoted depth-1 one, so the embedded Workspace.CachePoolable is never set by decoding and must
// not be read here — CachePoolable below is the caller's intent.
type registerWorkspaceRequest struct {
	workspace.Workspace
	CachePoolable *bool `json:"cache_poolable"`
}

// registerWorkspaceResponse states the pooling state the workspace ENDED UP IN, so a client can
// render it rather than assume. It reports what was recorded — not what was requested: an explicit
// true on an already-opted-out workspace is refused, and this says so.
type registerWorkspaceResponse struct {
	ID string `json:"id"`
	// CachePoolable: true means responses from this workspace may be served to OTHER tenants, and
	// this workspace may be served from theirs, once the operator's global pooling switch is on.
	CachePoolable bool `json:"cache_poolable"`
}

// newRegisterWorkspaceHandler serves POST /v1/workspaces — provisioning/registration of a workspace
// by id. Extracted VERBATIM from the inline run() closure so its route can be wrapped in
// requireAdmin (the cross-tenant IDOR fix): RegisterWorkspace is a blind upsert on the body-supplied
// id, so an ungated route let any non-admin overwrite another tenant's config. This handler is
// unchanged; the admin gate is applied at the route (main.go), matching the ~30 sibling
// control-plane routes. No money/ledger path is touched here.
func newRegisterWorkspaceHandler(reg registrar) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in registerWorkspaceRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		// Only pass a choice when the caller actually made one; silence must reach the manager as
		// silence so the new-workspace default applies.
		var opts []workspace.RegisterOption
		if in.CachePoolable != nil {
			opts = append(opts, workspace.WithCachePoolableChoice(*in.CachePoolable))
		}
		if err := reg.RegisterWorkspace(req.Context(), in.Workspace, opts...); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// Report the RECORDED consent, not the request. These differ whenever the workspace already
		// existed: its stored consent is preserved and an explicit true is refused.
		poolable, _ := reg.CachePoolableConsent(in.ID)
		writeJSONOK(w, http.StatusCreated, registerWorkspaceResponse{ID: in.ID, CachePoolable: poolable})
	}
}
