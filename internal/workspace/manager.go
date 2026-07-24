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

// LoggingPolicy controls how much of a request a workspace persists.
// metadata is the safe default — costs/tokens land in token_events but
// raw prompt text never does. full enables prompt_text capture for
// compliance, and none disables every observability write entirely
// (security checks still run on the hot path).
type LoggingPolicy string

const (
	LoggingFull     LoggingPolicy = "full"
	LoggingMetadata LoggingPolicy = "metadata"
	LoggingNone     LoggingPolicy = "none"
)

// normalizeLoggingPolicy maps unknown strings (including "") to the
// safe default so a misconfigured workspace never accidentally enables
// "full" logging.
func normalizeLoggingPolicy(p LoggingPolicy) LoggingPolicy {
	switch p {
	case LoggingFull, LoggingMetadata, LoggingNone:
		return p
	default:
		return LoggingMetadata
	}
}

// loggingPermissiveness ranks how much a policy persists (higher = more data
// retained = less private): none(0) < metadata(1) < full(2). The stale fail-closed
// clamp uses it to lower an over-permissive value toward the default — never raise it.
func loggingPermissiveness(p LoggingPolicy) int {
	switch normalizeLoggingPolicy(p) {
	case LoggingNone:
		return 0
	case LoggingFull:
		return 2
	default:
		return 1 // metadata
	}
}

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

	// Fail-closed-on-stale (consent/privacy): when the in-memory cache hasn't been
	// successfully reloaded within maxStaleness (a prolonged DB outage at this
	// replica), the consent/privacy accessors return their conservative floor
	// instead of a possibly-revoked permissive value. lastReload is stamped under mu
	// on every successful LoadAll swap. The override is DISABLED (fail-open) when
	// maxStaleness==0, pool==nil, or lastReload is zero ("never loaded") — so a
	// fresh/in-memory Manager never spuriously fails closed. now is injectable for tests.
	now          func() time.Time
	maxStaleness time.Duration // 0 = disabled; wired from LENS_WORKSPACE_MAX_STALENESS in main
	lastReload   time.Time     // zero until the first successful LoadAll
}

type Workspace struct {
	ID                  string        `json:"id"`
	Name                string        `json:"name"`
	CachePrefix         string        `json:"cache_prefix"`
	SpendLimitUSD       float64       `json:"spend_limit_usd"`
	AllowedModels       []string      `json:"allowed_models"`
	AllowedProviders    []string      `json:"allowed_providers"`
	MaxTokensPerRequest int           `json:"max_tokens_per_request"`
	MaxOutputTokens     int           `json:"max_output_tokens"`
	MaxInputTokens      int           `json:"max_input_tokens"`
	Active              bool          `json:"active"`
	LoggingPolicy       LoggingPolicy `json:"logging_policy"`
	DistillPolicy       DistillPolicy `json:"distill_policy"`
	CachePoolable       bool          `json:"cache_poolable"`
	CostOptimizeRouting bool          `json:"cost_optimize_routing"`
	DistillPoolable     bool          `json:"distill_poolable"`
	CreatedAt           time.Time     `json:"created_at"`
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
		now:        time.Now,
	}
}

const insertWorkspaceSQL = `INSERT INTO workspaces (
  id, name, cache_prefix, spend_limit_usd,
  allowed_models, allowed_providers, max_tokens_per_request,
  max_output_tokens, max_input_tokens, active, logging_policy, distill_policy,
  cache_poolable, distill_poolable, cost_optimize_routing
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
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
  logging_policy         = EXCLUDED.logging_policy,
  distill_policy         = EXCLUDED.distill_policy,
  -- cache_poolable is DELIBERATELY not updated on conflict: a re-registration must
  -- never change a workspace's cross-tenant pooling consent (symmetric flag; can't
  -- be granted/revoked retroactively). The stored value is preserved across the
  -- upsert; SetCachePoolable is the only path that changes it. This also guards a
  -- replica whose in-memory cache doesn't yet hold the row from re-defaulting it.
  distill_poolable       = EXCLUDED.distill_poolable,
  cost_optimize_routing  = EXCLUDED.cost_optimize_routing,
  updated_at             = NOW()
RETURNING cache_poolable`

const updateLoggingPolicySQL = `UPDATE workspaces
SET logging_policy = $2, updated_at = NOW()
WHERE id = $1`

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
	// Lifecycle is server-controlled, not the caller's to set. An omitted
	// `active` in the request body decodes to Go's zero value (false); writing
	// that over the DB's `DEFAULT true` made the row invisible to the boot
	// reload (LoadAll filters WHERE active=true), silently reverting a
	// LoggingNone customer's policy to the metadata default on restart (#129).
	// Force true: there is no deactivation flow today, so true is the only
	// correct value; if one is ever added, it must NOT go through this path.
	ws.Active = true
	// allowed_models/allowed_providers are NOT NULL in the schema; a nil slice
	// encodes as SQL NULL and 400s a minimal {id,name} registration — including
	// the boot default-workspace registration (#128). Default to empty (non-nil)
	// slices, which mean "no restriction" (enforcement is len()>0 && !contains).
	if ws.AllowedModels == nil {
		ws.AllowedModels = []string{}
	}
	if ws.AllowedProviders == nil {
		ws.AllowedProviders = []string{}
	}
	ws.LoggingPolicy = normalizeLoggingPolicy(ws.LoggingPolicy)
	ws.DistillPolicy = normalizeDistillPolicy(ws.DistillPolicy)

	// Cross-tenant cache pooling (cache_poolable) defaults ON for a NEW workspace,
	// but its consent is NEVER changed retroactively. The flag is SYMMETRIC — one
	// column gates both benefiting from and contributing to the shared cache — so a
	// blind re-POST must not silently (re-)pool an existing tenant's content. A new
	// workspace gets true (mirroring the 0106 column DEFAULT, since this INSERT
	// always supplies the column); an existing one keeps whatever it already had.
	// (bool can't distinguish an omitted body field from an explicit false, so an
	// at-registration opt-out isn't expressible here — a new workspace that wants
	// privacy opts out afterward via SetCachePoolable. The DB upsert preserves the
	// existing value too, guarding a replica whose cache doesn't yet hold the row.)
	stored := ws
	m.mu.Lock()
	if existing, ok := m.workspaces[ws.ID]; ok {
		stored.CachePoolable = existing.CachePoolable
	} else {
		stored.CachePoolable = true
	}
	m.workspaces[ws.ID] = &stored
	m.mu.Unlock()

	if m.pool != nil {
		// RETURNING gives back the cache_poolable the DB actually kept. On a new row that
		// is the new-workspace default (true); on a conflict it is the PRESERVED existing
		// value (the ON CONFLICT clause never overwrites it). Reconcile the in-memory flag
		// to it so a re-registration on a replica whose cache didn't hold the row — which
		// would otherwise apply the new default in memory — cannot re-pool an opted-out
		// workspace even transiently. The persisted opt-out boundary holds in memory too.
		var dbPoolable bool
		if err := m.pool.QueryRow(ctx, insertWorkspaceSQL,
			stored.ID, stored.Name, stored.CachePrefix, stored.SpendLimitUSD,
			stored.AllowedModels, stored.AllowedProviders, stored.MaxTokensPerRequest,
			stored.MaxOutputTokens, stored.MaxInputTokens, stored.Active, string(stored.LoggingPolicy),
			string(stored.DistillPolicy), stored.CachePoolable, stored.DistillPoolable, stored.CostOptimizeRouting,
		).Scan(&dbPoolable); err != nil {
			return fmt.Errorf("workspace: insert: %w", err)
		}
		if dbPoolable != stored.CachePoolable {
			m.mu.Lock()
			if cur, ok := m.workspaces[ws.ID]; ok {
				cur.CachePoolable = dbPoolable
			}
			m.mu.Unlock()
		}
	}
	return nil
}

// SetMaxStaleness configures the fail-closed bound: once the in-memory cache has
// gone this long without a SUCCESSFUL reload, the consent/privacy accessors return
// their conservative floor. 0 disables it (the default — a Manager fails closed on
// stale only when main wires LENS_WORKSPACE_MAX_STALENESS). Set once at startup.
func (m *Manager) SetMaxStaleness(d time.Duration) {
	m.mu.Lock()
	m.maxStaleness = d
	m.mu.Unlock()
}

// staleBeyondBoundLocked is the SINGLE predicate every consent/privacy accessor
// consults (never inlined — the one-definition discipline). Caller MUST hold at
// least the read lock. Fail-OPEN by construction: returns false (no override) when
// the override is disabled (maxStaleness<=0), when there is no DB to reload from
// (pool==nil), or before the first successful reload (lastReload zero) — the
// bootstrap guards, so a fresh or in-memory Manager never spuriously fails closed.
func (m *Manager) staleBeyondBoundLocked() bool {
	if m.pool == nil || m.maxStaleness <= 0 || m.lastReload.IsZero() {
		return false
	}
	return m.now().Sub(m.lastReload) > m.maxStaleness
}

// GetLoggingPolicy returns the workspace's logging policy or the safe metadata
// default when the workspace isn't registered. Hot-path code (proxy.serve) calls
// this on every request — lock-light, never reaches the DB. Stale beyond the bound
// it SURGICALLY clamps an over-permissive value (see below), never forcing none.
func (m *Manager) GetLoggingPolicy(wsID string) LoggingPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.workspaces[wsID]
	if !ok {
		return LoggingMetadata
	}
	cached := normalizeLoggingPolicy(ws.LoggingPolicy)
	// SURGICAL fail-closed (NOT a blunt floor): logging has no single conservative
	// direction — `none` protects a privacy tenant, `full` a compliance tenant. So
	// when the cache is stale past the bound, clamp ONLY a value MORE PERMISSIVE than
	// the default (full → metadata): revert an UN-CONFIRMED relaxation — the worst
	// case is a revoked `full` still capturing raw prompt_text — WITHOUT forcing
	// `none` on a tenant whose confirmed setting is full. Accepted residual: a stale
	// `metadata` secretly tightened to `none` is NOT forced to none (indistinguishable
	// from a confirmed metadata; forcing none would under-serve every metadata/
	// compliance tenant). effective ≤ cached always — never more permissive.
	if m.staleBeyondBoundLocked() && loggingPermissiveness(cached) > loggingPermissiveness(LoggingMetadata) {
		return LoggingMetadata
	}
	return cached
}

// SetLoggingPolicy updates the in-memory cache and the DB row. Policy
// changes take effect on the very next request — there's no
// per-request cache of the policy decision.
func (m *Manager) SetLoggingPolicy(ctx context.Context, wsID string, policy LoggingPolicy) error {
	policy = normalizeLoggingPolicy(policy)
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.LoggingPolicy = policy
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateLoggingPolicySQL, wsID, string(policy)); err != nil {
			return fmt.Errorf("workspace: update logging_policy: %w", err)
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
  max_output_tokens, max_input_tokens, active, logging_policy, distill_policy, cache_poolable, distill_poolable, cost_optimize_routing, created_at
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

	// Build a FRESH map OUTSIDE the lock, then publish it with a single pointer
	// swap under the write lock — making LoadAll safe to re-run on a reload ticker
	// (U7b). Properties this guarantees:
	//   - No torn read: readers hold RLock (GetLoggingPolicy/GetCachePoolable); the
	//     swap holds Lock, so a reader sees either the whole old map or the whole
	//     new one — never a half-built map (the cache-pooling privacy decision).
	//   - A failed build never swaps: a query/scan/rows error returns with the OLD
	//     map untouched (nothing half-built is ever published).
	//   - Deletions propagate: a workspace that left the result set (deactivated —
	//     loadAllSQL filters active=true — or deleted) is dropped by ABSENCE from
	//     the new map instead of lingering with a stale policy.
	next := make(map[string]*Workspace)
	for rows.Next() {
		var ws Workspace
		var policy, dpolicy string
		if err := rows.Scan(
			&ws.ID, &ws.Name, &ws.CachePrefix, &ws.SpendLimitUSD,
			&ws.AllowedModels, &ws.AllowedProviders, &ws.MaxTokensPerRequest,
			&ws.MaxOutputTokens, &ws.MaxInputTokens, &ws.Active, &policy, &dpolicy, &ws.CachePoolable, &ws.DistillPoolable, &ws.CostOptimizeRouting, &ws.CreatedAt,
		); err != nil {
			return fmt.Errorf("workspace: scan: %w", err) // old map intact — no swap
		}
		ws.LoggingPolicy = normalizeLoggingPolicy(LoggingPolicy(policy))
		ws.DistillPolicy = normalizeDistillPolicy(DistillPolicy(dpolicy))
		stored := ws
		next[ws.ID] = &stored
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("workspace: load all rows: %w", err) // old map intact — no swap
	}

	m.mu.Lock()
	m.workspaces = next
	m.lastReload = m.now() // a successful reload resets the staleness clock
	m.mu.Unlock()
	return nil
}

// Reload rebuilds the in-memory workspace cache from the DB via the build-then-swap
// LoadAll. Public + directly callable so a CRUD path can refresh immediately on the
// local node and tests can drive propagation deterministically (the U7c seam).
func (m *Manager) Reload(ctx context.Context) error { return m.LoadAll(ctx) }

// StartRefresh runs Reload on a fixed interval until ctx is cancelled, bounding
// cross-replica staleness of workspace config (logging policy, cache-pooling flag)
// to ≈interval. A transient DB error is logged, NOT fatal — build-then-swap keeps
// the old cache live and the next tick retries. Mirrors budgets.Service.StartRefresh.
// Intended to run once per process (a plain goroutine, not leader-gated: every
// replica must reload its own cache).
func (m *Manager) StartRefresh(ctx context.Context, interval time.Duration) {
	if m == nil || m.pool == nil {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := m.Reload(ctx); err != nil {
					slog.Warn("workspace: reload failed (old cache kept)", slog.String("err", err.Error()))
				}
			}
		}
	}()
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
