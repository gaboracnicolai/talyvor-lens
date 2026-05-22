package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultWorkspaceID = "default"

// pgxDB is the subset of *pgxpool.Pool that Manager needs. Tests pass nil
// pool — the in-memory map carries the entire policy decision for
// model/provider/token checks.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Manager struct {
	pool       pgxDB
	mu         sync.RWMutex
	workspaces map[string]*Workspace
}

type Workspace struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	CachePrefix         string    `json:"cache_prefix"`
	SpendLimitUSD       float64   `json:"spend_limit_usd"`
	AllowedModels       []string  `json:"allowed_models"`
	AllowedProviders    []string  `json:"allowed_providers"`
	MaxTokensPerRequest int       `json:"max_tokens_per_request"`
	MaxOutputTokens     int       `json:"max_output_tokens"`
	MaxInputTokens      int       `json:"max_input_tokens"`
	Active              bool      `json:"active"`
	CreatedAt           time.Time `json:"created_at"`
}

type WorkspacePolicy struct {
	Workspace *Workspace
	Violation string
	Allowed   bool
}

func New(pool *pgxpool.Pool) *Manager {
	// Guard the typed-nil interface trap.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Manager{
		pool:       db,
		workspaces: make(map[string]*Workspace),
	}
}

const insertWorkspaceSQL = `INSERT INTO workspaces (
  id, name, cache_prefix, spend_limit_usd,
  allowed_models, allowed_providers, max_tokens_per_request,
  max_output_tokens, max_input_tokens, active
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
  name                   = EXCLUDED.name,
  cache_prefix           = EXCLUDED.cache_prefix,
  spend_limit_usd        = EXCLUDED.spend_limit_usd,
  allowed_models         = EXCLUDED.allowed_models,
  allowed_providers      = EXCLUDED.allowed_providers,
  max_tokens_per_request = EXCLUDED.max_tokens_per_request,
  max_output_tokens      = EXCLUDED.max_output_tokens,
  max_input_tokens       = EXCLUDED.max_input_tokens,
  active                 = EXCLUDED.active,
  updated_at             = NOW()`

func (m *Manager) RegisterWorkspace(ctx context.Context, ws Workspace) error {
	if ws.ID == "" {
		return errors.New("workspace: ID required")
	}
	if ws.Name == "" {
		return errors.New("workspace: Name required")
	}
	if ws.CachePrefix == "" {
		ws.CachePrefix = "ws:" + ws.ID + ":"
	}
	if ws.CreatedAt.IsZero() {
		ws.CreatedAt = time.Now().UTC()
	}

	stored := ws
	m.mu.Lock()
	m.workspaces[ws.ID] = &stored
	m.mu.Unlock()

	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, insertWorkspaceSQL,
			stored.ID, stored.Name, stored.CachePrefix, stored.SpendLimitUSD,
			stored.AllowedModels, stored.AllowedProviders, stored.MaxTokensPerRequest,
			stored.MaxOutputTokens, stored.MaxInputTokens, stored.Active,
		); err != nil {
			return fmt.Errorf("workspace: insert: %w", err)
		}
	}
	return nil
}

// ListWorkspaces returns copies of every registered workspace. Used by
// admin endpoints to surface the full tenant list.
func (m *Manager) ListWorkspaces() []*Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Workspace, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		copy := *ws
		out = append(out, &copy)
	}
	return out
}

func (m *Manager) GetWorkspace(id string) (*Workspace, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.workspaces[id]
	if !ok {
		return nil, false
	}
	// Return a copy so callers can't mutate our map.
	copy := *ws
	return &copy, true
}

func (m *Manager) ExtractWorkspaceID(r *http.Request) string {
	if id := r.Header.Get("X-Talyvor-Workspace"); id != "" {
		return id
	}
	return defaultWorkspaceID
}

// permissive returns a permissive policy used when the workspace is not
// found. Per the spec, "use 'default' workspace if not found, which allows
// everything" — and if the default workspace itself is unregistered, we
// fall through to a permissive policy so requests don't fail.
func (m *Manager) permissive(ws *Workspace) WorkspacePolicy {
	return WorkspacePolicy{Workspace: ws, Allowed: true}
}

func (m *Manager) lookup(id string) *Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ws, ok := m.workspaces[id]; ok {
		c := *ws
		return &c
	}
	if ws, ok := m.workspaces[defaultWorkspaceID]; ok {
		c := *ws
		return &c
	}
	return nil
}

func (m *Manager) CheckPolicy(ctx context.Context, wsID, provider, model string, inputTokens int) WorkspacePolicy {
	ws := m.lookup(wsID)
	if ws == nil {
		// No workspace AND no default registered — fall through to
		// permissive so a stripped-down install still serves traffic.
		return m.permissive(nil)
	}

	if len(ws.AllowedProviders) > 0 && !containsString(ws.AllowedProviders, provider) {
		return WorkspacePolicy{
			Workspace: ws,
			Violation: fmt.Sprintf("provider %q not allowed for workspace %q", provider, ws.ID),
			Allowed:   false,
		}
	}
	if len(ws.AllowedModels) > 0 && !containsString(ws.AllowedModels, model) {
		return WorkspacePolicy{
			Workspace: ws,
			Violation: fmt.Sprintf("model %q not allowed for workspace %q", model, ws.ID),
			Allowed:   false,
		}
	}
	if ws.MaxTokensPerRequest > 0 && inputTokens > ws.MaxTokensPerRequest {
		return WorkspacePolicy{
			Workspace: ws,
			Violation: fmt.Sprintf("request exceeds %d token limit for workspace %q", ws.MaxTokensPerRequest, ws.ID),
			Allowed:   false,
		}
	}

	// Spend limit is the only DB-backed check. Skip entirely if there's no
	// pool or the workspace doesn't enforce a limit — we'd rather under-
	// enforce than fail-closed on a transient DB error.
	if ws.SpendLimitUSD > 0 && m.pool != nil {
		spend, err := m.monthlySpend(ctx, ws.ID)
		if err != nil {
			slog.Warn("workspace: monthly spend query failed",
				slog.String("workspace_id", ws.ID),
				slog.String("err", err.Error()),
			)
		} else if spend >= ws.SpendLimitUSD {
			return WorkspacePolicy{
				Workspace: ws,
				Violation: fmt.Sprintf("workspace %q spend limit ($%.2f) exceeded", ws.ID, ws.SpendLimitUSD),
				Allowed:   false,
			}
		}
	}

	return WorkspacePolicy{Workspace: ws, Allowed: true}
}

const monthlySpendSQL = `SELECT COALESCE(SUM(cost_usd), 0)
FROM token_events
WHERE workspace_id = $1
  AND created_at > NOW() - INTERVAL '30 days'`

func (m *Manager) monthlySpend(ctx context.Context, wsID string) (float64, error) {
	var spend float64
	if err := m.pool.QueryRow(ctx, monthlySpendSQL, wsID).Scan(&spend); err != nil {
		return 0, err
	}
	return spend, nil
}

func (m *Manager) ScopedCacheKey(wsID, baseKey string) string {
	m.mu.RLock()
	ws, ok := m.workspaces[wsID]
	m.mu.RUnlock()
	if ok {
		return ws.CachePrefix + baseKey
	}
	// Unknown workspace: synthesize the same prefix shape so isolation
	// still holds for transient/unregistered workspace IDs.
	return "ws:" + wsID + ":" + baseKey
}

const loadAllSQL = `SELECT id, name, cache_prefix, spend_limit_usd,
  allowed_models, allowed_providers, max_tokens_per_request,
  max_output_tokens, max_input_tokens, active, created_at
FROM workspaces
WHERE active = true`

func (m *Manager) LoadAll(ctx context.Context) error {
	if m.pool == nil {
		return nil
	}
	rows, err := m.pool.Query(ctx, loadAllSQL)
	if err != nil {
		return fmt.Errorf("workspace: load all: %w", err)
	}
	defer rows.Close()

	m.mu.Lock()
	defer m.mu.Unlock()
	for rows.Next() {
		var ws Workspace
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.CachePrefix, &ws.SpendLimitUSD,
			&ws.AllowedModels, &ws.AllowedProviders, &ws.MaxTokensPerRequest,
			&ws.MaxOutputTokens, &ws.MaxInputTokens, &ws.Active, &ws.CreatedAt,
		); err != nil {
			return fmt.Errorf("workspace: scan: %w", err)
		}
		stored := ws
		m.workspaces[ws.ID] = &stored
	}
	return rows.Err()
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
