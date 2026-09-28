package workspace

import (
	"context"
	"errors"
	"fmt"
)

// B17.1 — SYNTHETIC WORKSPACES: TEST ACCOUNTS THAT CAN NEVER TOUCH REAL MONEY OR REAL USERS.
//
// A synthetic workspace is created only by CreateSynthetic, which the operator-key routes call. It
// pools (so the harness exercises the pool), but its pooled answers live in a partition of their own
// (the proxy folds the flag into the request fingerprint), so it never shares with a real workspace
// in either direction. The flag is never cleared.

// markSyntheticSQL flips the flag and pooling on in ONE statement, after the workspace was
// registered with pooling OFF: a failure between the two leaves a workspace that shares nothing,
// never a real-looking one that pools.
const markSyntheticSQL = `UPDATE workspaces SET synthetic = true, cache_poolable = true, updated_at = NOW() WHERE id = $1`

const listSyntheticSQL = `SELECT id FROM workspaces WHERE synthetic AND active ORDER BY id`

// GetSynthetic reports whether wsID is a synthetic workspace, as recorded. Unlike GetCachePoolable it
// is not clamped on staleness: a synthetic workspace stays synthetic, and a stale cache already
// turns pooling off for everyone.
func (m *Manager) GetSynthetic(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.workspaces[wsID]
	return ok && ws.Synthetic
}

// CreateSynthetic registers a new synthetic workspace. It refuses an id that already exists, so a
// real workspace can never be turned into a synthetic one.
func (m *Manager) CreateSynthetic(ctx context.Context, id, name string) error {
	if m.pool == nil {
		return errors.New("workspace: synthetic workspaces need the database")
	}
	if _, exists := m.GetWorkspace(id); exists {
		return fmt.Errorf("workspace: %s already exists", id)
	}
	var n int
	if err := m.pool.QueryRow(ctx, `SELECT count(*) FROM workspaces WHERE id = $1`, id).Scan(&n); err != nil {
		return fmt.Errorf("workspace: check %s: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("workspace: %s already exists", id)
	}
	if err := m.RegisterWorkspace(ctx, Workspace{ID: id, Name: name, Active: true}, WithCachePoolableChoice(false)); err != nil {
		return err
	}
	if _, err := m.pool.Exec(ctx, markSyntheticSQL, id); err != nil {
		return fmt.Errorf("workspace: mark %s synthetic: %w", id, err)
	}
	m.mu.Lock()
	if ws, ok := m.workspaces[id]; ok {
		ws.Synthetic, ws.CachePoolable = true, true
	}
	m.mu.Unlock()
	return nil
}

// ListSynthetic returns every active synthetic workspace, from the database.
func (m *Manager) ListSynthetic(ctx context.Context) ([]string, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, listSyntheticSQL)
	if err != nil {
		return nil, fmt.Errorf("workspace: list synthetic: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
