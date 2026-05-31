// Package tenant owns per-workspace configuration, API keys, and
// guard-rail enforcement (spending caps, model allowlists,
// scope checks). Sits alongside the existing `auth` package —
// `auth` issues Lens-admin keys; `tenant` issues workspace-
// scoped keys with their own quota envelope.
//
// All the heavy lifting (DB schema, bcrypt hashing, async spend
// cache) is collected here. The model allowlist check `CheckAllowed`
// returns an error the proxy can surface to the client without
// round-tripping to Postgres. (The live spending gate is
// budgets.Service.CheckBudget on the proxy hot path; SpendTracker
// here only tracks the cached current-month spend for the admin API.)

package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// ─── constants ────────────────────────────────────

const (
	// KeyPrefix is the literal "tlv_ws_" header on every
	// generated workspace key. The first 8 hex chars of the
	// random body follow, giving a 15-char displayable prefix
	// that's safe to log / show in dashboards.
	KeyPrefix = "tlv_ws_"

	// keyHexLen is the random portion length (32 hex chars =
	// 16 bytes of entropy — well over the bar for an API key).
	keyHexLen = 32

	// PrefixLookupLen is what we slice into for the prefix-
	// indexed DB lookup before bcrypt comparison.
	PrefixLookupLen = len(KeyPrefix) + 8

	// BcryptCost is the per-hash cost factor. 10 is the spec'd
	// default and matches the cost the standard library calls
	// "DefaultCost"; ~100ms on modern hardware.
	BcryptCost = 10

	// MonthlyCacheTTL is how long SpendTracker trusts an
	// in-process monthly-spend snapshot before re-querying the
	// DB. 5 minutes balances "react quickly to a quota change"
	// against "don't hammer Postgres per request".
	MonthlyCacheTTL = 5 * time.Minute

	// DefaultRetentionDays is the policy when a workspace config
	// leaves retention_days = 0.
	DefaultRetentionDays = 90
)

// ValidScopes is the closed set of API-key scopes. Anything
// outside this list is rejected at create time so we don't end
// up with stray scopes lingering in the database.
var ValidScopes = map[string]bool{
	"proxy":     true,
	"analytics": true,
	"admin":     true,
}

// ─── types ────────────────────────────────────────

// WorkspaceConfig is the per-workspace policy bundle. Zero
// values for the quota fields mean "unlimited" — the proxy
// treats them as "skip the check entirely".
type WorkspaceConfig struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	APIKeys          []WorkspaceAPIKey `json:"api_keys,omitempty"`
	SpendingCapUSD   float64   `json:"spending_cap_usd"`
	MonthlyBudget    float64   `json:"monthly_budget"`
	RateLimitRPM     int       `json:"rate_limit_rpm"`
	RateLimitTPM     int       `json:"rate_limit_tpm"`
	AllowedModels    []string  `json:"allowed_models"`
	AllowedProviders []string  `json:"allowed_providers"`
	LogLevel         string    `json:"log_level"`
	RetentionDays    int       `json:"retention_days"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// WorkspaceAPIKey is the persisted view (no plaintext). The
// raw key is returned exactly once from CreateAPIKey and never
// stored.
type WorkspaceAPIKey struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	KeyHash     string     `json:"-"` // never marshalled
	KeyPrefix   string     `json:"key_prefix"`
	Name        string     `json:"name"`
	Scopes      []string   `json:"scopes"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ─── errors ───────────────────────────────────────

// Sentinel errors. The proxy maps these to specific HTTP status
// codes / messages; tests use `errors.Is` to assert intent.
var (
	ErrModelNotAllowed    = errors.New("tenant: model not in workspace allowlist")
	ErrProviderNotAllowed = errors.New("tenant: provider not in workspace allowlist")
	ErrInvalidKey         = errors.New("tenant: invalid or unknown API key")
	ErrKeyExpired         = errors.New("tenant: API key has expired")
	ErrInvalidScope       = errors.New("tenant: invalid scope (must be one of: proxy, analytics, admin)")
)

// ─── pgxDB / Store skeleton ──────────────────────

// pgxDB is the subset of *pgxpool.Pool we depend on. Tests use
// pgxmock; nil pool short-circuits read paths to "not found"
// and silently drops write paths so unit tests can exercise the
// pure logic.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store owns the workspace_configs + workspace_api_keys tables.
type Store struct {
	pool pgxDB
}

func NewStore(pool *pgxpool.Pool) *Store {
	// Avoid the typed-nil interface trap: (*pgxpool.Pool)(nil)
	// stored in pgxDB compares != nil but panics on call.
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(pool pgxDB) *Store {
	return &Store{pool: pool}
}

// ─── config CRUD ─────────────────────────────────

const upsertConfigSQL = `
INSERT INTO workspace_configs (
    id, name, spending_cap, monthly_budget,
    rate_limit_rpm, rate_limit_tpm,
    allowed_models, allowed_providers,
    log_level, retention_days, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, COALESCE($11, NOW()), NOW()
)
ON CONFLICT (id) DO UPDATE SET
    name              = EXCLUDED.name,
    spending_cap      = EXCLUDED.spending_cap,
    monthly_budget    = EXCLUDED.monthly_budget,
    rate_limit_rpm    = EXCLUDED.rate_limit_rpm,
    rate_limit_tpm    = EXCLUDED.rate_limit_tpm,
    allowed_models    = EXCLUDED.allowed_models,
    allowed_providers = EXCLUDED.allowed_providers,
    log_level         = EXCLUDED.log_level,
    retention_days    = EXCLUDED.retention_days,
    updated_at        = NOW()`

// UpsertConfig writes (or replaces) the workspace's policy
// bundle. Validation lives in the function rather than as a
// pre-step so callers can't accidentally bypass it.
func (s *Store) UpsertConfig(ctx context.Context, c WorkspaceConfig) error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("tenant: workspace id required")
	}
	if c.RetentionDays < 0 {
		return errors.New("tenant: retention_days must be ≥ 0")
	}
	if c.LogLevel != "" && c.LogLevel != "all" && c.LogLevel != "errors" && c.LogLevel != "none" {
		return fmt.Errorf("tenant: invalid log_level %q", c.LogLevel)
	}
	if s.pool == nil {
		return nil
	}
	var createdAt any
	if !c.CreatedAt.IsZero() {
		createdAt = c.CreatedAt
	}
	_, err := s.pool.Exec(ctx, upsertConfigSQL,
		c.ID, c.Name, c.SpendingCapUSD, c.MonthlyBudget,
		c.RateLimitRPM, c.RateLimitTPM,
		c.AllowedModels, c.AllowedProviders,
		stringsOr(c.LogLevel, "all"),
		intOrDefault(c.RetentionDays, DefaultRetentionDays),
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("tenant: upsert config: %w", err)
	}
	return nil
}

const getConfigSQL = `
SELECT id, name, spending_cap, monthly_budget,
       rate_limit_rpm, rate_limit_tpm,
       allowed_models, allowed_providers,
       log_level, retention_days, created_at, updated_at
FROM workspace_configs WHERE id = $1`

// GetConfig returns (nil, nil) when the workspace has no
// explicit config — callers should treat that as "use defaults"
// rather than as an error.
func (s *Store) GetConfig(ctx context.Context, workspaceID string) (*WorkspaceConfig, error) {
	if s.pool == nil {
		return nil, nil
	}
	row := s.pool.QueryRow(ctx, getConfigSQL, workspaceID)
	var c WorkspaceConfig
	err := row.Scan(
		&c.ID, &c.Name, &c.SpendingCapUSD, &c.MonthlyBudget,
		&c.RateLimitRPM, &c.RateLimitTPM,
		&c.AllowedModels, &c.AllowedProviders,
		&c.LogLevel, &c.RetentionDays,
		&c.CreatedAt, &c.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: get config: %w", err)
	}
	return &c, nil
}

// ─── API keys ────────────────────────────────────

// GenerateKey produces a fresh `tlv_ws_<32 hex>` token. Pure
// helper exposed so tests and admin scripts can generate keys
// outside the Store.CreateAPIKey path when needed.
func GenerateKey() (string, string, error) {
	buf := make([]byte, keyHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("tenant: read random: %w", err)
	}
	raw := KeyPrefix + hex.EncodeToString(buf)
	prefix := raw[:PrefixLookupLen]
	return raw, prefix, nil
}

const insertKeySQL = `
INSERT INTO workspace_api_keys (
    workspace_id, key_hash, key_prefix, name, scopes, expires_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, NOW())
RETURNING id, created_at`

// CreateAPIKey issues a fresh workspace-scoped key. Returns
// (rawKey, metadata, err) — the raw key is returned exactly
// once. The plaintext is never stored; only the bcrypt hash +
// the displayable prefix land in the DB.
func (s *Store) CreateAPIKey(
	ctx context.Context,
	workspaceID, name string,
	scopes []string,
	expiresAt *time.Time,
) (string, *WorkspaceAPIKey, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return "", nil, errors.New("tenant: workspace id required")
	}
	if err := ValidateScopes(scopes); err != nil {
		return "", nil, err
	}
	raw, prefix, err := GenerateKey()
	if err != nil {
		return "", nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(raw), BcryptCost)
	if err != nil {
		return "", nil, fmt.Errorf("tenant: bcrypt: %w", err)
	}
	key := &WorkspaceAPIKey{
		WorkspaceID: workspaceID,
		KeyHash:     string(hash),
		KeyPrefix:   prefix,
		Name:        name,
		Scopes:      append([]string{}, scopes...),
		ExpiresAt:   expiresAt,
	}
	if s.pool == nil {
		// In-memory mode (tests): synthesise an ID + timestamp
		// so the returned struct mirrors what the DB path
		// produces.
		key.ID = newPseudoID()
		key.CreatedAt = time.Now().UTC()
		return raw, key, nil
	}
	row := s.pool.QueryRow(ctx, insertKeySQL,
		workspaceID, key.KeyHash, key.KeyPrefix, key.Name,
		scopes, expiresAt,
	)
	if err := row.Scan(&key.ID, &key.CreatedAt); err != nil {
		return "", nil, fmt.Errorf("tenant: insert key: %w", err)
	}
	return raw, key, nil
}

const findByPrefixSQL = `
SELECT id, workspace_id, key_hash, key_prefix, name, scopes,
       last_used_at, expires_at, created_at
FROM workspace_api_keys
WHERE key_prefix = $1`

const touchKeySQL = `UPDATE workspace_api_keys SET last_used_at = NOW() WHERE id = $1`

// ValidateAPIKey checks the supplied raw key against the prefix-
// indexed candidates and runs bcrypt.CompareHashAndPassword.
// Returns ErrInvalidKey when nothing matches and ErrKeyExpired
// when the matched key has an `expires_at` in the past.
//
// Touches last_used_at on a successful match so admins can see
// stale keys in the management UI.
func (s *Store) ValidateAPIKey(ctx context.Context, raw string) (*WorkspaceAPIKey, error) {
	if len(raw) < PrefixLookupLen || !strings.HasPrefix(raw, KeyPrefix) {
		return nil, ErrInvalidKey
	}
	if s.pool == nil {
		return nil, ErrInvalidKey
	}
	prefix := raw[:PrefixLookupLen]
	rows, err := s.pool.Query(ctx, findByPrefixSQL, prefix)
	if err != nil {
		return nil, fmt.Errorf("tenant: lookup key: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k WorkspaceAPIKey
		if err := rows.Scan(
			&k.ID, &k.WorkspaceID, &k.KeyHash, &k.KeyPrefix,
			&k.Name, &k.Scopes, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tenant: scan key: %w", err)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(k.KeyHash), []byte(raw)); err != nil {
			// Wrong hash — keep looking; multiple keys could
			// share a prefix in the (vanishingly rare) collision
			// case, and we never want to leak which one matched.
			continue
		}
		if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()) {
			return nil, ErrKeyExpired
		}
		// Touch last_used_at — best-effort, errors are logged
		// but don't fail the validation.
		_, _ = s.pool.Exec(ctx, touchKeySQL, k.ID)
		now := time.Now().UTC()
		k.LastUsedAt = &now
		return &k, nil
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant: rows: %w", err)
	}
	return nil, ErrInvalidKey
}

const revokeKeySQL = `DELETE FROM workspace_api_keys WHERE id = $1`

// RevokeAPIKey removes the key by ID. Idempotent — returns nil
// even if no row was affected.
func (s *Store) RevokeAPIKey(ctx context.Context, keyID string) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, revokeKeySQL, keyID); err != nil {
		return fmt.Errorf("tenant: revoke key: %w", err)
	}
	return nil
}

const listKeysSQL = `
SELECT id, workspace_id, key_hash, key_prefix, name, scopes,
       last_used_at, expires_at, created_at
FROM workspace_api_keys
WHERE workspace_id = $1
ORDER BY created_at DESC`

// ListAPIKeys returns every key for the workspace. KeyHash is
// preserved on the struct for internal use; the field is
// `json:"-"` so it never leaks to API consumers.
func (s *Store) ListAPIKeys(ctx context.Context, workspaceID string) ([]WorkspaceAPIKey, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, listKeysSQL, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("tenant: list keys: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceAPIKey
	for rows.Next() {
		var k WorkspaceAPIKey
		if err := rows.Scan(
			&k.ID, &k.WorkspaceID, &k.KeyHash, &k.KeyPrefix,
			&k.Name, &k.Scopes, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("tenant: scan key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ValidateScopes returns ErrInvalidScope when any scope is
// outside ValidScopes. Empty slice is OK (request-with-no-
// permissions; the caller decides how to treat that).
func ValidateScopes(scopes []string) error {
	for _, s := range scopes {
		if !ValidScopes[s] {
			return fmt.Errorf("%w: %q", ErrInvalidScope, s)
		}
	}
	return nil
}

// ─── allowlist enforcement ───────────────────────

// CheckAllowed returns:
//   - ErrModelNotAllowed when the model isn't in AllowedModels
//   - ErrProviderNotAllowed when the provider isn't in AllowedProviders
//   - nil when both lists are empty (or both lists allow the call)
//
// Either list being empty means "all allowed" — so a workspace
// with only AllowedProviders set still lets any model through
// for those providers.
func CheckAllowed(config *WorkspaceConfig, model, provider string) error {
	if config == nil {
		return nil
	}
	if len(config.AllowedProviders) > 0 && !contains(config.AllowedProviders, provider) {
		return fmt.Errorf("%w: %s", ErrProviderNotAllowed, provider)
	}
	if len(config.AllowedModels) > 0 && !contains(config.AllowedModels, model) {
		return fmt.Errorf("%w: %s", ErrModelNotAllowed, model)
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// ─── spend tracker ────────────────────────────────

const monthSpendSQL = `
SELECT COALESCE(SUM(cost_usd), 0)
FROM token_events
WHERE workspace_id = $1
  AND created_at >= date_trunc('month', NOW())`

// spendCacheEntry is one cached read of the current month's
// spend. Refreshed when older than MonthlyCacheTTL or when a
// RecordSpend call has pushed the cached total past the cap.
type spendCacheEntry struct {
	spent    float64
	fetched  time.Time
}

// SpendTracker keeps the cached current-month spend per workspace
// (in-process, 5-minute TTL + explicit RecordSpend / InvalidateCache)
// so the admin API can read it without a Postgres query per call.
// Note: it does NOT enforce the spend cap — that gate is
// budgets.Service.CheckBudget on the proxy hot path.
type SpendTracker struct {
	store *Store
	mu    sync.RWMutex
	cache map[string]*spendCacheEntry
}

func NewSpendTracker(store *Store) *SpendTracker {
	return &SpendTracker{
		store: store,
		cache: map[string]*spendCacheEntry{},
	}
}

// CurrentSpend exposes the cached running total for the API.
// Wraps `currentSpend` for callers that want the value without
// a cap check.
func (st *SpendTracker) CurrentSpend(ctx context.Context, workspaceID string) (float64, error) {
	if st == nil || st.store == nil {
		return 0, nil
	}
	return st.currentSpend(ctx, workspaceID)
}

func (st *SpendTracker) currentSpend(ctx context.Context, workspaceID string) (float64, error) {
	st.mu.RLock()
	e, ok := st.cache[workspaceID]
	st.mu.RUnlock()
	if ok && time.Since(e.fetched) < MonthlyCacheTTL {
		return e.spent, nil
	}
	// Cache miss — query Postgres. Best-effort: a query error
	// returns 0 + nil (callers treat current-spend as a soft signal).
	if st.store == nil || st.store.pool == nil {
		return 0, nil
	}
	row := st.store.pool.QueryRow(ctx, monthSpendSQL, workspaceID)
	var sum float64
	if err := row.Scan(&sum); err != nil {
		return 0, fmt.Errorf("tenant: month spend: %w", err)
	}
	st.mu.Lock()
	st.cache[workspaceID] = &spendCacheEntry{spent: sum, fetched: time.Now()}
	st.mu.Unlock()
	return sum, nil
}

// RecordSpend updates the cached running total. The DB write
// already happens via the existing alerts pipeline (token_events
// table is the source of truth); this method exists so the
// in-process cache stays close to reality without waiting for
// the 5-minute TTL.
func (st *SpendTracker) RecordSpend(_ context.Context, workspaceID string, costUSD float64) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	e, ok := st.cache[workspaceID]
	if !ok {
		// Lazy init — let the next currentSpend cache-miss repopulate
		// from the DB. We could backfill here but it'd hold the lock
		// across a Postgres query.
		return
	}
	e.spent += costUSD
}

// InvalidateCache drops the cached snapshot for `workspaceID`
// so the next CurrentSpend call re-queries Postgres. Useful when
// the admin API changes the config.
func (st *SpendTracker) InvalidateCache(workspaceID string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	delete(st.cache, workspaceID)
	st.mu.Unlock()
}

// ─── small helpers ────────────────────────────────

func stringsOr(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func intOrDefault(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

// newPseudoID is the no-DB fallback for CreateAPIKey. We don't
// pull in a UUID library for this — the timestamp + 8 random
// hex chars is plenty for the in-memory test path.
func newPseudoID() string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("key_%d_%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// ─── small constructor helpers ───────────────────

// SortedScopes returns a copy of the slice in stable order so
// JSON output is deterministic across writes.
func SortedScopes(scopes []string) []string {
	out := append([]string{}, scopes...)
	sort.Strings(out)
	return out
}
