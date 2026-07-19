package main

import (
	"context"
	"encoding/json"
	"net/http"
)

// lxcGranter is the admin-grant seam (*economy.DualTokenStore satisfies it via GrantLXC). Extracted so
// the POST /v1/admin/lxc/grant handler is testable and can be wrapped in requireAdmin at the route.
type lxcGranter interface {
	GrantLXC(ctx context.Context, workspaceID string, lxcAmount int64, reason string, metadata map[string]interface{}) (int64, error)
}

// adminLXCGrantRequest is the body of POST /v1/admin/lxc/grant. AmountULXC is integer µLXC — no float.
type adminLXCGrantRequest struct {
	WorkspaceID string `json:"workspace_id"`
	AmountULXC  int64  `json:"amount_ulxc"`
	Reason      string `json:"reason"`
}

// newAdminLXCGrantHandler serves POST /v1/admin/lxc/grant — an ADMIN-ONLY comped-LXC grant that funds a
// workspace so a closed trial can onboard WITHOUT a Stripe purchase (a fresh workspace has 0 LXC and
// otherwise no funding path, so it cannot transact). It credits through economy.GrantLXC — the
// canonical atomic ledger-row + balance move — and NEVER writes lxc_ledger/lxc_balances directly. The
// row is recorded under LXCTypeGrant ("admin_grant"), never "purchase", so a comp is always
// distinguishable from paid revenue. The route is admin-gated (requireAdmin) and default-off behind
// LENS_ADMIN_LXC_GRANT_ENABLED at main.go (off ⇒ the route is not registered at all).
func newAdminLXCGrantHandler(g lxcGranter) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var in adminLXCGrantRequest
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if in.WorkspaceID == "" {
			writeJSONErr(w, http.StatusBadRequest, "workspace_id is required")
			return
		}
		if in.AmountULXC <= 0 {
			writeJSONErr(w, http.StatusBadRequest, "amount_ulxc must be a positive integer (µLXC)")
			return
		}
		reason := in.Reason
		if reason == "" {
			reason = "admin comped LXC grant"
		}
		// Self-describing: the row already carries LXCTypeGrant; the metadata restates the comp and
		// the operator note so a ledger row read in isolation is never mistaken for a paid purchase.
		meta := map[string]interface{}{"kind": "admin_grant", "grant_note": in.Reason}
		newBal, err := g.GrantLXC(req.Context(), in.WorkspaceID, in.AmountULXC, reason, meta)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]interface{}{
			"workspace_id":     in.WorkspaceID,
			"granted_ulxc":     in.AmountULXC,
			"new_balance_ulxc": newBal,
		})
	}
}
