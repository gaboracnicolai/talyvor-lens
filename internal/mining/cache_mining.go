// Package mining is the LENS-token compute-mining engine. This
// file lands the cache-mining track: workspaces earn LENS when
// the responses they cached get served to other workspaces.
// Future files in this package (storage_mining.go, model_mining.go,
// etc.) will share the LedgerStore implemented here.

package mining

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/metrics"
)

// ─── constants ───────────────────────────────────

// Token rates (LENS). Tuned so cross-workspace contributions
// pay ~10× the trivial own-cache case — encourages anonymised
// sharing without giving teams a way to farm themselves.
const (
	CacheHitSameWorkspace  = 0.001
	CacheHitCrossWorkspace = 0.010
	SemanticCacheHit       = 0.005
)

// Ledger transaction types — keep in sync with the type column
// CHECK constraint we'd add in a future migration (we leave the
// column as free-text for now so new mining tracks don't need a
// migration).
const (
	TypeCacheMine = "cache_mine"
	TypeSpend     = "spend"
	TypeTransfer  = "transfer"
)

// ─── types ───────────────────────────────────────

// LedgerEntry mirrors one row of lens_token_ledger.
type LedgerEntry struct {
	ID           string                 `json:"id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Amount       float64                `json:"amount"`
	BalanceAfter float64                `json:"balance_after"`
	Type         string                 `json:"type"`
	Description  string                 `json:"description"`
	Metadata     map[string]interface{} `json:"metadata"`
	CreatedAt    time.Time              `json:"created_at"`
}

// BalanceSnapshot mirrors one row of lens_token_balances.
type BalanceSnapshot struct {
	WorkspaceID    string    `json:"workspace_id"`
	Balance        float64   `json:"balance"`
	LifetimeEarned float64   `json:"lifetime_earned"`
	LifetimeSpent  float64   `json:"lifetime_spent"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CacheMiningStats is the response shape for the
// /v1/workspaces/:wsID/tokens/mining/cache endpoint.
type CacheMiningStats struct {
	WorkspaceID        string  `json:"workspace_id"`
	CurrentBalance     float64 `json:"current_balance"`
	LifetimeEarned     float64 `json:"lifetime_earned"`
	CacheHitsServed    int     `json:"cache_hits_served"`
	CacheHitsBenefited int     `json:"cache_hits_benefited"`
	EstimatedMonthly   float64 `json:"estimated_monthly"`
}

// ─── errors ──────────────────────────────────────

// ErrInsufficientBalance is the typed error LedgerStore.Debit
// returns when the workspace doesn't have enough LENS to cover
// the debit. Callers `errors.Is` against it to map to 402 Payment
// Required at the HTTP boundary.
var ErrInsufficientBalance = errors.New("mining: insufficient balance")

// ─── pgxDB shim ──────────────────────────────────

// pgxDB is the subset of *pgxpool.Pool the store needs. Letting
// tests substitute pgxmock keeps integration tests cheap.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ─── LedgerStore ─────────────────────────────────

// LedgerStore is the persistence layer for every token movement
// in Lens. All mutators run inside a transaction so the ledger
// insert and the balance update are atomically consistent.
type LedgerStore struct {
	pool pgxDB
}

// NewLedgerStore wraps a real *pgxpool.Pool.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newLedgerStore(db)
}

// newLedgerStore is the test-friendly constructor (accepts the
// pgxmock interface).
func newLedgerStore(db pgxDB) *LedgerStore {
	return &LedgerStore{pool: db}
}

// PgxDB is the public alias for the internal pgx interface. Lets
// sister packages (e.g. internal/economy) build a LedgerStore
// against a pgxmock pool in tests without exporting unrelated
// internals.
type PgxDB = pgxDB

// NewLedgerStoreForTesting wraps an arbitrary pgxDB-shaped pool
// (typically a pgxmock.PgxPoolIface) so tests in other packages
// can construct a LedgerStore without owning a real
// *pgxpool.Pool. Production code should use NewLedgerStore.
func NewLedgerStoreForTesting(db PgxDB) *LedgerStore {
	return newLedgerStore(db)
}

// Credit adds `amount` LENS to a workspace. Append-only — no row
// is ever updated, only inserted. The balance table is upserted
// inside the same tx so reads always see a consistent snapshot.
func (s *LedgerStore) Credit(
	ctx context.Context,
	workspaceID string,
	amount float64,
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: credit amount must be positive")
	}
	if err := s.apply(ctx, workspaceID, amount, txType, description, metadata, true); err != nil {
		return err
	}
	// A credit is LENS entering circulation (mining rewards). Transfers move
	// between workspaces via Transfer (not Credit), so this doesn't double-count.
	metrics.MintedTokens(amount)
	return nil
}

// Debit removes `amount` LENS from a workspace. Returns
// ErrInsufficientBalance when the workspace doesn't have enough
// to cover it — the tx rolls back so no partial state leaks.
func (s *LedgerStore) Debit(
	ctx context.Context,
	workspaceID string,
	amount float64,
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: debit amount must be positive")
	}
	return s.apply(ctx, workspaceID, amount, txType, description, metadata, false)
}

// apply is the shared transactional path Credit + Debit funnel
// through. The `add` flag toggles the sign — that keeps the
// SQL identical regardless of direction.
func (s *LedgerStore) apply(
	ctx context.Context,
	workspaceID string,
	amount float64,
	txType, description string,
	metadata map[string]interface{},
	add bool,
) error {
	if s.pool == nil {
		// No DB — silently succeed so tests that skip the DB
		// path can still exercise higher-level mining logic.
		return nil
	}
	// Observe the ledger-write hot path (begin→commit). This is the latency
	// the LensTokenLedgerSlow alert guards.
	start := time.Now()
	defer func() { metrics.ObserveLedgerWrite(time.Since(start)) }()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Two-step pessimistic lock: ensure the row exists without touching
	// updated_at on every write, then acquire an explicit FOR UPDATE lock.
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return fmt.Errorf("mining: ensure balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	var bal, earned, spent float64
	if err := row.Scan(&bal, &earned, &spent); err != nil {
		return fmt.Errorf("mining: read balance: %w", err)
	}

	var delta float64
	if add {
		delta = amount
		earned += amount
	} else {
		if bal < amount {
			return ErrInsufficientBalance
		}
		delta = -amount
		spent += amount
	}
	newBal := bal + delta

	metaBuf, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}
	if string(metaBuf) == "null" {
		metaBuf = []byte("{}")
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, newBal, txType, description, metaBuf); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1
	`, workspaceID, newBal, earned, spent); err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}

	return tx.Commit(ctx)
}

// GetBalance returns 0.0 (not an error) for a workspace with no
// ledger history. That matches the spec — new workspaces are a
// supported case, not an exceptional one.
func (s *LedgerStore) GetBalance(ctx context.Context, workspaceID string) (float64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT balance FROM lens_token_balances WHERE workspace_id = $1
	`, workspaceID)
	var b float64
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("mining: read balance: %w", err)
	}
	return b, nil
}

// GetSnapshot returns the full balance row (with lifetime
// totals). Returns a zero-value snapshot (not nil) for a fresh
// workspace so callers don't have to nil-check.
func (s *LedgerStore) GetSnapshot(ctx context.Context, workspaceID string) (BalanceSnapshot, error) {
	if s.pool == nil {
		return BalanceSnapshot{WorkspaceID: workspaceID}, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT workspace_id, balance, lifetime_earned, lifetime_spent, updated_at
		FROM lens_token_balances WHERE workspace_id = $1
	`, workspaceID)
	var s2 BalanceSnapshot
	if err := row.Scan(&s2.WorkspaceID, &s2.Balance, &s2.LifetimeEarned, &s2.LifetimeSpent, &s2.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return BalanceSnapshot{WorkspaceID: workspaceID}, nil
		}
		return BalanceSnapshot{}, fmt.Errorf("mining: read snapshot: %w", err)
	}
	return s2, nil
}

// ─── Transfer / Burn (Batch 3) ──────────────────

// MinTransferAmount is the floor for Transfer. Spec-mandated —
// below this we risk dust attacks polluting the ledger.
const MinTransferAmount = 0.001

// TypeTransfer / TypeBurn are the ledger row types for the
// Batch-3 economy primitives.
const (
	TypeTransferOut = "transfer_out"
	TypeTransferIn  = "transfer_in"
	TypeBurn        = "burn"
)

// Transfer atomically debits `from` and credits `to`. Both
// ledger rows + both balance updates happen inside one
// transaction so a partial failure can't drop or duplicate LENS.
func (s *LedgerStore) Transfer(
	ctx context.Context,
	fromWorkspace, toWorkspace string,
	amount float64,
	description string,
) error {
	if amount < MinTransferAmount {
		return fmt.Errorf("mining: transfer amount must be ≥ %v", MinTransferAmount)
	}
	if fromWorkspace == "" || toWorkspace == "" {
		return errors.New("mining: from / to workspace required")
	}
	if fromWorkspace == toWorkspace {
		return errors.New("mining: cannot transfer to self")
	}
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin transfer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Debit `from`.
	fromBal, fromEarned, fromSpent, err := readBalance(ctx, tx, fromWorkspace)
	if err != nil {
		return err
	}
	if fromBal < amount {
		return ErrInsufficientBalance
	}
	fromBalNew := fromBal - amount
	fromSpentNew := fromSpent + amount
	meta := map[string]interface{}{"counterparty": toWorkspace}
	if err := insertLedgerRow(ctx, tx, fromWorkspace, -amount, fromBalNew,
		TypeTransferOut, description, meta); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, fromWorkspace, fromBalNew, fromEarned, fromSpentNew); err != nil {
		return err
	}

	// Credit `to`.
	toBal, toEarned, toSpent, err := readBalance(ctx, tx, toWorkspace)
	if err != nil {
		return err
	}
	toBalNew := toBal + amount
	toEarnedNew := toEarned + amount
	metaIn := map[string]interface{}{"counterparty": fromWorkspace}
	if err := insertLedgerRow(ctx, tx, toWorkspace, amount, toBalNew,
		TypeTransferIn, description, metaIn); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, toWorkspace, toBalNew, toEarnedNew, toSpent); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Burn removes LENS from a workspace's balance and from
// circulating supply. Used when a workspace spends LENS on
// upstream AI calls (LENS-paid mode). Burns are irreversible.
func (s *LedgerStore) Burn(ctx context.Context, workspaceID string, amount float64, description string) error {
	if amount <= 0 {
		return errors.New("mining: burn amount must be positive")
	}
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin burn: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	bal, earned, spent, err := readBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if bal < amount {
		return ErrInsufficientBalance
	}
	balNew := bal - amount
	spentNew := spent + amount
	if err := insertLedgerRow(ctx, tx, workspaceID, -amount, balNew, TypeBurn, description, nil); err != nil {
		return err
	}
	if err := writeBalance(ctx, tx, workspaceID, balNew, earned, spentNew); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetTotalSupply returns the all-time minted LENS — the sum of
// every credit-side ledger row that came from a mining track.
// (Transfers don't mint new LENS so they're excluded.)
func (s *LedgerStore) GetTotalSupply(ctx context.Context) (float64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE amount > 0 AND type IN ($1, $2, $3, $4, $5)`,
		TypeCacheMine, TypeComputeMine, TypeEmbeddingMine,
		TypeAnnotationMine, TypePatternMine)
	var n float64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: total supply: %w", err)
	}
	return n, nil
}

// GetCirculatingSupply = total minted - total burned. The
// difference is what's currently in workspace wallets + staked.
//
// "Burned" counts BOTH plain burns (TypeBurn) AND slashed stake
// (TypeStakeSlash) — a slash destroys collateral, reducing supply
// (PoVI Part 3). Without counting slashes, supply would be overstated
// after a slash, and supply feeds the LXC conversion math.
func (s *LedgerStore) GetCirculatingSupply(ctx context.Context) (float64, error) {
	total, err := s.GetTotalSupply(ctx)
	if err != nil {
		return 0, err
	}
	if s.pool == nil {
		return total, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE type IN ($1, $2)`,
		TypeBurn, TypeStakeSlash)
	var burned float64
	if err := row.Scan(&burned); err != nil {
		return 0, fmt.Errorf("mining: burned: %w", err)
	}
	return total - burned, nil
}

// GetTotalBurned returns the cumulative LENS removed from supply — both plain
// burns (TypeBurn) AND slashed stake (TypeStakeSlash). Counting slashes keeps
// the economy-stats display (GetEconomyStats = total − burned) consistent with
// the slash-aware GetCirculatingSupply.
func (s *LedgerStore) GetTotalBurned(ctx context.Context) (float64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE type IN ($1, $2)`,
		TypeBurn, TypeStakeSlash)
	var n float64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: total burned: %w", err)
	}
	return n, nil
}

// ─── tx helpers shared by Transfer + Burn ───────

// readBalance + writeBalance + insertLedgerRow factor out the SQL
// the Transfer / Burn flows share. They take a pgx.Tx so the
// caller controls transactional semantics.

func readBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, earned, spent float64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("mining: ensure balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &earned, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("mining: read balance: %w", err)
	}
	return
}

func writeBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, earned, spent float64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, earned, spent)
	if err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}
	return nil
}

func insertLedgerRow(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter float64,
	txType, description string, metadata map[string]interface{}) error {
	metaBuf, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}
	if string(metaBuf) == "null" {
		metaBuf = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, balanceAfter, txType, description, metaBuf); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}
	return nil
}

// GetHistory returns the ledger entries for a workspace, newest
// first. Bounds: limit ≤ 200, offset ≥ 0.
func (s *LedgerStore) GetHistory(ctx context.Context, workspaceID string, limit, offset int) ([]LedgerEntry, error) {
	if s.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, amount, balance_after, type, description, metadata, created_at
		FROM lens_token_ledger
		WHERE workspace_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("mining: query history: %w", err)
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Amount, &e.BalanceAfter,
			&e.Type, &e.Description, &meta, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("mining: scan history: %w", err)
		}
		if len(meta) > 0 {
			_ = json.Unmarshal(meta, &e.Metadata)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── CacheMiner ──────────────────────────────────

// CacheMiner is the LENS earner for the cache-contribution
// track. Wraps a LedgerStore + the cross-workspace toggle.
type CacheMiner struct {
	ledger          *LedgerStore
	crossWorkspace  bool

	// mu guards the in-process counters used by GetMiningStats
	// when there's no DB pool wired (test path).
	mu              sync.Mutex
	served          map[string]int
	benefited       map[string]int
}

// NewCacheMiner builds a miner. `crossWorkspaceEnabled` mirrors
// LENS_CACHE_SHARING_ENABLED — when false, the cross-workspace
// rate is never applied (even if a different workspace gets the
// hit, only the same-workspace tiny reward is credited, because
// in non-sharing mode the cache is still being treated as a
// workspace-private artefact).
func NewCacheMiner(ledger *LedgerStore, crossWorkspaceEnabled bool) *CacheMiner {
	return &CacheMiner{
		ledger:         ledger,
		crossWorkspace: crossWorkspaceEnabled,
		served:         map[string]int{},
		benefited:      map[string]int{},
	}
}

// RecordCacheHit credits the cache owner for serving a hit.
// hitType ∈ {"exact", "semantic"}. The two workspace IDs may be
// equal (workspace serving its own cache).
func (m *CacheMiner) RecordCacheHit(
	ctx context.Context,
	cacheOwnerWorkspace string,
	requestWorkspace string,
	hitType string,
) error {
	if cacheOwnerWorkspace == "" {
		// No owner recorded — older cache entries from before
		// owner tracking landed. Skip (no one to credit).
		return nil
	}

	// Track per-workspace counters for GetMiningStats — the DB
	// query is the canonical source, but holding a small
	// in-process counter lets stats endpoints work even before
	// the first DB flush.
	m.mu.Lock()
	m.served[cacheOwnerWorkspace]++
	if requestWorkspace != "" && requestWorkspace != cacheOwnerWorkspace {
		m.benefited[requestWorkspace]++
	}
	m.mu.Unlock()

	earning := m.earnFor(cacheOwnerWorkspace, requestWorkspace, hitType)
	if earning <= 0 {
		return nil
	}

	meta := map[string]interface{}{
		"hit_type":             hitType,
		"request_workspace_id": requestWorkspace,
	}
	desc := fmt.Sprintf("cache hit (%s) served to %s", hitType, requestWorkspace)
	return m.ledger.Credit(ctx, cacheOwnerWorkspace, earning, TypeCacheMine, desc, meta)
}

// earnFor encapsulates the rate-selection rules — same workspace
// gets the tiny reward; cross-workspace gets the bigger reward
// when sharing is enabled; semantic hits get the middle rate
// even cross-workspace.
func (m *CacheMiner) earnFor(owner, requester, hitType string) float64 {
	if owner == requester || requester == "" || !m.crossWorkspace {
		return CacheHitSameWorkspace
	}
	if hitType == "semantic" {
		return SemanticCacheHit
	}
	return CacheHitCrossWorkspace
}

// GetMiningStats summarises the workspace's cache-mining activity.
// EstimatedMonthly is a 30× projection of the average daily earn
// over the last 30 days — naive but it's what the dashboard wants.
func (m *CacheMiner) GetMiningStats(ctx context.Context, workspaceID string) (*CacheMiningStats, error) {
	snap, err := m.ledger.GetSnapshot(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	served := m.served[workspaceID]
	benefited := m.benefited[workspaceID]
	m.mu.Unlock()

	// EstimatedMonthly: lifetimeEarned across a guessed
	// onboarding-to-now window. Without a per-day breakdown we
	// project from "rough average across the workspace lifetime",
	// floored at 30 days so a brand-new workspace doesn't
	// extrapolate one big hit into a giant projection.
	monthly := 0.0
	if !snap.UpdatedAt.IsZero() {
		days := time.Since(snap.UpdatedAt).Hours()/24 + 1
		if days < 30 {
			days = 30
		}
		monthly = snap.LifetimeEarned / days * 30
	}

	return &CacheMiningStats{
		WorkspaceID:        workspaceID,
		CurrentBalance:     snap.Balance,
		LifetimeEarned:     snap.LifetimeEarned,
		CacheHitsServed:    served,
		CacheHitsBenefited: benefited,
		EstimatedMonthly:   monthly,
	}, nil
}

// Rates returns the public rate table — backs the
// /v1/tokens/rates endpoint.
func Rates() map[string]float64 {
	return map[string]float64{
		"cache_hit_same":  CacheHitSameWorkspace,
		"cache_hit_cross": CacheHitCrossWorkspace,
		"semantic_hit":    SemanticCacheHit,
	}
}
