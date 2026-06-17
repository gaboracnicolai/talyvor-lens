package workspace

import (
	"context"
	"fmt"
)

// DistillPolicy controls whether the request-path DISTILL integration converts
// a document in this workspace's requests. It mirrors LoggingPolicy. The safe
// default is DistillDisabled: distillation is inert until an admin enables it,
// so the live request path stays byte-for-byte unchanged for every workspace
// that has not opted in.
type DistillPolicy string

const (
	// DistillDisabled (the default) never distills — fully inert.
	DistillDisabled DistillPolicy = "disabled"
	// DistillOptIn distills a document request ONLY when it also sends the
	// X-Talyvor-Distill header (per-request opt-in).
	DistillOptIn DistillPolicy = "opt_in"
	// DistillAlways distills whenever a document is present (no header needed).
	DistillAlways DistillPolicy = "always"
)

// normalizeDistillPolicy maps unknown/empty values to the safe default
// (disabled) so a misconfigured workspace never silently turns distillation on.
func normalizeDistillPolicy(p DistillPolicy) DistillPolicy {
	switch p {
	case DistillDisabled, DistillOptIn, DistillAlways:
		return p
	default:
		return DistillDisabled
	}
}

const updateDistillPolicySQL = `UPDATE workspaces
SET distill_policy = $2, updated_at = NOW()
WHERE id = $1`

// GetDistillPolicy returns the workspace's distill policy, or the safe
// DistillDisabled default when the workspace isn't registered. Hot-path code
// (proxy.serve) calls this on every request — lock-light, never reaches the DB.
func (m *Manager) GetDistillPolicy(wsID string) DistillPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Fail closed on prolonged staleness: stop distilling when the policy can't be
	// confirmed (an unconfirmed opt-in could be stale-permissive).
	if m.staleBeyondBoundLocked() {
		return DistillDisabled
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return normalizeDistillPolicy(ws.DistillPolicy)
	}
	return DistillDisabled
}

// SetDistillPolicy updates the in-memory cache and the DB row. Policy changes
// take effect on the very next request.
func (m *Manager) SetDistillPolicy(ctx context.Context, wsID string, policy DistillPolicy) error {
	policy = normalizeDistillPolicy(policy)
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.DistillPolicy = policy
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateDistillPolicySQL, wsID, string(policy)); err != nil {
			return fmt.Errorf("workspace: update distill_policy: %w", err)
		}
	}
	return nil
}
