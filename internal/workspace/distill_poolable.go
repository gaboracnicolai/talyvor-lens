package workspace

import (
	"context"
	"fmt"
)

// distill_poolable.go is the per-tenant opt-in for cross-tenant DISTILL-cache
// sharing — the document-artifact analogue of cache_poolable.go. It mirrors the
// same shape: a lock-light Get for the hot path and a Set that writes through to
// the DB. The safe default is FALSE (private): a workspace's distill artifacts
// are served only to itself until an admin opts it in AND the global switch
// (LENS_DISTILL_POOLABLE_ENABLED) is on. A distill artifact is document-derived,
// a more sensitive disclosure than a prompt/response pair, so this is a SEPARATE
// consent from cache_poolable, not a reuse of it.

const updateDistillPoolableSQL = `UPDATE workspaces
SET distill_poolable = $2, updated_at = NOW()
WHERE id = $1`

// GetDistillPoolable reports whether the workspace participates in the shared
// distill cache. Returns the safe FALSE default for an unregistered or unset
// workspace, so cross-tenant distill sharing never turns on implicitly.
// Lock-light, never reaches the DB (the distill produce/serve path calls it).
func (m *Manager) GetDistillPoolable(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ws, ok := m.workspaces[wsID]; ok {
		return ws.DistillPoolable
	}
	return false
}

// SetDistillPoolable updates the in-memory flag and the DB row; the change takes
// effect on the very next request. Errors when the workspace isn't registered.
func (m *Manager) SetDistillPoolable(ctx context.Context, wsID string, poolable bool) error {
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.DistillPoolable = poolable
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateDistillPoolableSQL, wsID, poolable); err != nil {
			return fmt.Errorf("workspace: update distill_poolable: %w", err)
		}
	}
	return nil
}
