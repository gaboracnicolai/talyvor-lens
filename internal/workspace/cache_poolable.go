package workspace

import (
	"context"
	"fmt"
)

// cache_poolable.go is the per-tenant opt-in for the Phase-2 Stage 2.0
// shared-cache governance gate (exact cache). It mirrors distill_policy.go: a
// lock-light Get for the hot path and a Set that writes through to the DB. The
// safe default is FALSE (private) — a workspace participates in the shared cache
// only after an admin explicitly opts it in AND the global switch is on.

const updateCachePoolableSQL = `UPDATE workspaces
SET cache_poolable = $2, updated_at = NOW()
WHERE id = $1`

// GetCachePoolable reports whether the workspace participates in the shared
// exact cache. Returns the safe FALSE default for an unregistered or unset
// workspace, so pooling never turns on implicitly. proxy.serve calls this per
// request — lock-light, never reaches the DB.
func (m *Manager) GetCachePoolable(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Fail closed on prolonged staleness: pause cross-tenant pooling when consent
	// can't be confirmed (a revoke on another replica may be unpropagated).
	if m.staleBeyondBoundLocked() {
		return false
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return ws.CachePoolable
	}
	return false
}

// SetCachePoolable updates the in-memory flag and the DB row; the change takes
// effect on the very next request. Errors when the workspace isn't registered.
func (m *Manager) SetCachePoolable(ctx context.Context, wsID string, poolable bool) error {
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.CachePoolable = poolable
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateCachePoolableSQL, wsID, poolable); err != nil {
			return fmt.Errorf("workspace: update cache_poolable: %w", err)
		}
	}
	return nil
}
