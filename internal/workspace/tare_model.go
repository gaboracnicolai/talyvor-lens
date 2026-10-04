package workspace

import (
	"context"
	"fmt"
)

// TareModel is whether the workspace has opted in to Tare phase 2a (B27.35) — the compression model
// that drops words from prose every phase-1 reducer refused. LOSSY, so OFF unless the workspace turns
// it on; it runs only where GetTarePolicy also lets Tare run.

const updateTareModelSQL = `UPDATE workspaces
SET tare_model = $2, updated_at = NOW()
WHERE id = $1`

// GetTareModel reports the opt-in. False for an unregistered workspace or a stale cache: every
// failure direction is OFF. Hot path — never reaches the DB.
func (m *Manager) GetTareModel(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.staleBeyondBoundLocked() {
		return false
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return ws.TareModel
	}
	return false
}

// SetTareModel updates the in-memory cache and the DB row. Takes effect on the next request.
func (m *Manager) SetTareModel(ctx context.Context, wsID string, on bool) error {
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.TareModel = on
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateTareModelSQL, wsID, on); err != nil {
			return fmt.Errorf("workspace: update tare_model: %w", err)
		}
	}
	return nil
}
