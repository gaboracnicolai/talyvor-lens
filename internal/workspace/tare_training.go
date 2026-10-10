package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
)

// TareTraining is whether the workspace lets Tare phase 2b — a compressor Talyvor trains on its own traffic
// (B27.36) — learn from its prose. OFF for every workspace until an owner turns it on (0243), and separate from
// Sharing and from TareModel: no switch turns on another. Talyvor's synthetic test workspaces are collected from
// without it; nothing else is.

// One statement, so the switch, its record and the forgetting land together or not at all.
const setTareTrainingSQL = `WITH upd AS (
  UPDATE workspaces SET tare_training = $2, updated_at = NOW() WHERE id = $1 RETURNING id
), sw AS (
  INSERT INTO tare_training_switches (workspace_id, enabled, changed_by, on_behalf_of) SELECT id, $2, $3, $4 FROM upd
  RETURNING changed_at
), forgot AS (
  DELETE FROM tare_training_traces WHERE workspace_id = $1 AND NOT $2 AND EXISTS (SELECT 1 FROM upd)
)
SELECT changed_at FROM sw`

// The EXISTS reads the opt-in from the row, not this replica's cache, so a switch-off on another replica stops
// collection here at once.
const addTareTraceSQL = `INSERT INTO tare_training_traces (workspace_id, text, text_sha256)
SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM workspaces WHERE id = $1 AND (tare_training OR synthetic))
ON CONFLICT (workspace_id, text_sha256) DO NOTHING`

// Joined on the opt-in as it stands, so a trace a lagging replica wrote after a switch-off is never trained on.
const tareTrainingSetSQL = `SELECT t.workspace_id, t.text FROM tare_training_traces t
JOIN workspaces w ON w.id = t.workspace_id
WHERE w.tare_training OR w.synthetic
ORDER BY t.id`

// TareTrace is one collected text and the workspace it came from.
type TareTrace struct {
	WorkspaceID string
	Text        string
}

// GetTareTraining reports the opt-in. False for an unregistered workspace or a stale cache.
func (m *Manager) GetTareTraining(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.staleBeyondBoundLocked() {
		return false
	}
	ws, ok := m.workspaces[wsID]
	return ok && ws.TareTraining
}

// CollectsTareTraining reports whether the workspace's prose may be collected at all: it opted in, or it is one of
// Talyvor's synthetic test workspaces. Every failure direction is OFF. Hot path — never reaches the DB.
func (m *Manager) CollectsTareTraining(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.staleBeyondBoundLocked() {
		return false
	}
	ws, ok := m.workspaces[wsID]
	return ok && (ws.TareTraining || ws.Synthetic)
}

// SetTareTraining switches the opt-in and records who did it — the authenticated caller, and the person it acted
// for if it named one — and when. Switched off, the workspace's collected traces are deleted in the same
// statement. Returns when the switch was recorded (now, without a DB).
func (m *Manager) SetTareTraining(ctx context.Context, wsID string, on bool, by, onBehalfOf string) (time.Time, error) {
	if by == "" {
		return time.Time{}, fmt.Errorf("workspace: who switched tare_training is required")
	}
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.TareTraining = on
	}
	m.mu.Unlock()
	if !ok {
		return time.Time{}, fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool == nil {
		return m.now(), nil
	}
	var at time.Time
	if err := m.pool.QueryRow(ctx, setTareTrainingSQL, wsID, on, by, onBehalfOf).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("workspace: update tare_training: %w", err)
	}
	return at, nil
}

// AddTareTrace keeps text for training if the workspace collects, as the DB has it now. The same text twice is kept once.
func (m *Manager) AddTareTrace(ctx context.Context, wsID, text string) error {
	if m.pool == nil {
		return nil
	}
	sum := sha256.Sum256([]byte(text))
	if _, err := m.pool.Exec(ctx, addTareTraceSQL, wsID, text, hex.EncodeToString(sum[:])); err != nil {
		return fmt.Errorf("workspace: add tare trace: %w", err)
	}
	return nil
}

// RecordTareTrace is AddTareTrace off the request path: a lost trace costs a training example, never a request.
func (m *Manager) RecordTareTrace(wsID, text string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.AddTareTrace(ctx, wsID, text); err != nil {
			slog.Warn("tare: training trace not kept", "workspace", wsID, "error", err)
		}
	}()
}

// TareTrainingSet is every trace a training run may read — the only way to read them.
func (m *Manager) TareTrainingSet(ctx context.Context) ([]TareTrace, error) {
	if m.pool == nil {
		return nil, nil
	}
	rows, err := m.pool.Query(ctx, tareTrainingSetSQL)
	if err != nil {
		return nil, fmt.Errorf("workspace: tare training set: %w", err)
	}
	defer rows.Close()
	var out []TareTrace
	for rows.Next() {
		var t TareTrace
		if err := rows.Scan(&t.WorkspaceID, &t.Text); err != nil {
			return nil, fmt.Errorf("workspace: tare training set: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
