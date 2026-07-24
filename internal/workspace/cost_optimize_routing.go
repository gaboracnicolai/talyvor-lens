package workspace

import (
	"context"
	"fmt"
)

// cost_optimize_routing.go is the per-tenant CONSENT to cost-optimised routing on
// CONCRETE (explicitly named) models. It mirrors cache_poolable.go: a lock-light
// Get for the hot path and a Set that writes through to the DB.
//
// The safe default is FALSE (honour the named model). A workspace's explicitly
// named model is served exactly as requested — never downgraded — unless the
// workspace has opted in here. This encodes the founder's rule: quality shall not
// be compromised without consent. The "auto" pseudo-model and the
// X-Talyvor-Auto-Route header are a SEPARATE, per-request delegation and route
// regardless of this flag (see proxy.routingDelegated).

const updateCostOptimizeRoutingSQL = `UPDATE workspaces
SET cost_optimize_routing = $2, updated_at = NOW()
WHERE id = $1`

// GetCostOptimizeRouting reports whether the workspace consented to cost-optimised
// routing on concrete models. Returns the safe FALSE default for an unregistered
// or unset workspace, so a named model is never downgraded implicitly. proxy.serve
// calls this per request — lock-light, never reaches the DB.
func (m *Manager) GetCostOptimizeRouting(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Fail closed on prolonged staleness: honour the named model when consent
	// can't be confirmed (an unconfirmed opt-in could be stale-permissive).
	if m.staleBeyondBoundLocked() {
		return false
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return ws.CostOptimizeRouting
	}
	return false
}

// SetCostOptimizeRouting updates the in-memory flag and the DB row; the change
// takes effect on the very next request. Errors when the workspace isn't registered.
func (m *Manager) SetCostOptimizeRouting(ctx context.Context, wsID string, enabled bool) error {
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.CostOptimizeRouting = enabled
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateCostOptimizeRoutingSQL, wsID, enabled); err != nil {
			return fmt.Errorf("workspace: update cost_optimize_routing: %w", err)
		}
	}
	return nil
}
