package workspace

import (
	"context"
	"fmt"
)

// TarePolicy controls whether the request path runs Tare (internal/tare) — the context-reduction
// layer — on this workspace's requests. B6.5. It mirrors DistillPolicy and CompressionPolicy: a
// stored policy plus a per-request header, X-Talyvor-Tare.
//
// ⚠ OFF BY DEFAULT. Tare's JSON and log reducers are lossless, but its code trimmer is LOSSY: it
// replaces function bodies with an announced elision. A workspace turns it on itself.
type TarePolicy string

const (
	// TareDisabled never reduces — fully inert. The default.
	TareDisabled TarePolicy = "disabled"
	// TareOptIn reduces ONLY when the request also carries X-Talyvor-Tare: true.
	TareOptIn TarePolicy = "opt_in"
	// TareAlways reduces every request (no header needed).
	TareAlways TarePolicy = "always"
)

// DefaultTarePolicy is what a workspace gets when it sets NO policy.
const DefaultTarePolicy = TareDisabled

// normalizeTarePolicy honours a valid value, resolves EMPTY to the default, and fails anything
// else SAFE to TareDisabled, so a typo can never start reducing requests.
func normalizeTarePolicy(p TarePolicy) TarePolicy {
	switch p {
	case TareDisabled, TareOptIn, TareAlways:
		return p
	case "":
		return DefaultTarePolicy
	default:
		return TareDisabled
	}
}

const updateTarePolicySQL = `UPDATE workspaces
SET tare_policy = $2, updated_at = NOW()
WHERE id = $1`

// GetTarePolicy returns the workspace's Tare policy, or TareDisabled when the workspace is not
// registered or the cache is stale. Hot path — never reaches the DB.
func (m *Manager) GetTarePolicy(wsID string) TarePolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.staleBeyondBoundLocked() {
		return TareDisabled
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return normalizeTarePolicy(ws.TarePolicy)
	}
	return TareDisabled
}

// SetTarePolicy updates the in-memory cache and the DB row. Takes effect on the next request.
func (m *Manager) SetTarePolicy(ctx context.Context, wsID string, policy TarePolicy) error {
	policy = normalizeTarePolicy(policy)
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.TarePolicy = policy
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateTarePolicySQL, wsID, string(policy)); err != nil {
			return fmt.Errorf("workspace: update tare_policy: %w", err)
		}
	}
	return nil
}
