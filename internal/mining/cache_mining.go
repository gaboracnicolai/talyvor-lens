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
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/metrics"
)

// ─── constants ───────────────────────────────────

// Token rates in µLENS (SEC-2: integer smallest-unit). Tuned so
// cross-workspace contributions pay ~10× the trivial own-cache case
// — encourages anonymised sharing without giving teams a way to farm
// themselves. 1_000 µLENS = 0.001 LENS.
const (
	CacheHitSameWorkspace  int64 = 1_000  // 0.001 LENS
	CacheHitCrossWorkspace int64 = 10_000 // 0.010 LENS
	SemanticCacheHit       int64 = 5_000  // 0.005 LENS
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

// LedgerEntry mirrors one row of lens_token_ledger. Amount/BalanceAfter are
// integer µLENS (SEC-2); the JSON keys carry the explicit _ulens unit suffix so
// no consumer mistakes the 1e6-scaled integer for a float LENS value.
type LedgerEntry struct {
	ID           string                 `json:"id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Amount       int64                  `json:"amount_ulens"`
	BalanceAfter int64                  `json:"balance_after_ulens"`
	Type         string                 `json:"type"`
	Description  string                 `json:"description"`
	Metadata     map[string]interface{} `json:"metadata"`
	CreatedAt    time.Time              `json:"created_at"`
}

// BalanceSnapshot mirrors one row of lens_token_balances. Balances are integer
// µLENS (SEC-2); JSON keys carry the explicit _ulens unit suffix.
type BalanceSnapshot struct {
	WorkspaceID    string    `json:"workspace_id"`
	Balance        int64     `json:"balance_ulens"`
	LifetimeEarned int64     `json:"lifetime_earned_ulens"`
	LifetimeSpent  int64     `json:"lifetime_spent_ulens"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CacheMiningStats is the response shape for the
// /v1/workspaces/:wsID/tokens/mining/cache endpoint. LENS amounts are integer
// µLENS (SEC-2); JSON keys carry the explicit _ulens unit suffix.
type CacheMiningStats struct {
	WorkspaceID        string `json:"workspace_id"`
	CurrentBalance     int64  `json:"current_balance_ulens"`
	LifetimeEarned     int64  `json:"lifetime_earned_ulens"`
	CacheHitsServed    int    `json:"cache_hits_served"`
	CacheHitsBenefited int    `json:"cache_hits_benefited"`
	EstimatedMonthly   int64  `json:"estimated_monthly_ulens"`
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
	// verifier is the U6 verified-to-earn gate. nil ⇒ no gate (tests + pre-flip
	// behave exactly as before). Checked inside applyTx/heldInner for mint-type
	// credits only — see mint_gate.go.
	verifier MintVerifier
	// mintRateCap is the U6 PR2 per-workspace rolling-window minted-LENS ceiling
	// in µLENS (<=0 ⇒ off); mintRateWindow is the window (default 24h). Enforced
	// inside applyTx/heldInner AFTER the balance FOR UPDATE — see checkMintRateCap.
	mintRateCap    int64
	mintRateWindow time.Duration
	// reputationGate is the P1 #9 reputation-bonded-minting flag. nil/false ⇒ no-op (mint path
	// byte-identical). When on, applyTx/CreditHeldTx scale a bonded mint by f(R) and gate it to 0
	// below the access floor — an ADDITIVE constraint downstream of verifyEarn (mint_gate.go).
	reputationGate func() bool

	// driftHaircut is KE-2: a REDUCE-ONLY multiplier in [floor, 1.0] applied AFTER the reputation factor,
	// derived from Keel's HARDENED idiosyncratic drift finding for the workspace. nil ⇒ no-op (byte-identical).
	// It can only LOWER a bonded mint (never increase, never below the floor, never burn/slash). Reads via the
	// caller's tx so mining imports no keel. See mint_gate.go:reputationBondedAmount.
	driftHaircut func(ctx context.Context, tx pgx.Tx, workspaceID string) (float64, error)
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
	amount int64, // µLENS
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
	metrics.MintedTokens(MicroToFloat(amount))
	return nil
}

// Debit removes `amount` LENS from a workspace. Returns
// ErrInsufficientBalance when the workspace doesn't have enough
// to cover it — the tx rolls back so no partial state leaks.
func (s *LedgerStore) Debit(
	ctx context.Context,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: debit amount must be positive")
	}
	return s.apply(ctx, workspaceID, amount, txType, description, metadata, false)
}

// applyTx contains all the SQL for a single credit or debit within a
// caller-supplied transaction. It does NOT call Begin, Commit, or Rollback —
// the caller owns the transaction boundary. This makes it composable: multiple
// ledger operations can share one transaction and roll back atomically together.
//
// Use Credit / Debit for standalone operations.
// Use CreditTx / DebitTx when multiple operations must be atomic together.
func (s *LedgerStore) applyTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
	add bool,
) error {
	// Observe the ledger-write hot path. This is the latency the
	// LensTokenLedgerSlow alert guards.
	start := time.Now()
	defer func() { metrics.ObserveLedgerWrite(time.Since(start)) }()

	// U6 Sybil floor: a mint-type credit only proceeds for a verified-to-earn
	// workspace. No-op for debits (add=false), conservation credits (non-mint
	// txType), and when no verifier is wired. Runs on THIS tx so a block
	// (ErrEarnNotVerified) rolls the whole mint back — no ledger row, no balance
	// change, no metrics.
	if add {
		if err := s.verifyEarn(ctx, tx, workspaceID, txType); err != nil {
			return err
		}
		// P1 #9: reputation bond — DOWNSTREAM of the U6 floor (compose, never bypass). For a bonded
		// mint type with the gate on, scale `amount` by f(R) and gate to ErrReputationFloor below
		// the access floor. No-op (amount unchanged, no read) when off or non-bonded → byte-identical.
		eff, rerr := s.reputationBondedAmount(ctx, tx, workspaceID, txType, amount, metadata)
		if rerr != nil {
			return rerr
		}
		amount = eff // the rest of the credit (delta, earned, rate-cap, ledger row) uses the effective amount
	}

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
	var bal, earned, spent int64
	if err := row.Scan(&bal, &earned, &spent); err != nil {
		return fmt.Errorf("mining: read balance: %w", err)
	}

	var delta int64
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

	// U6 PR2: rolling-window mint rate cap. Only for mint-type credits, and
	// placed AFTER the FOR UPDATE above so concurrent same-workspace mints
	// serialize on the balance row and the SUM sees prior committed mints
	// (exact, no race). A block (ErrMintRateCapExceeded) rolls the whole mint
	// back — no ledger row, no balance change, no metrics.
	if add {
		if err := s.checkMintRateCap(ctx, tx, workspaceID, txType, amount); err != nil {
			return err
		}
	}

	// dbjson.JSONB encodes as JSON text on both pgx protocols — a raw []byte
	// is inferred as bytea under the SimpleProtocol that LENS_DB_PGBOUNCER
	// forces and rejected by jsonb with 22P02 (#133).
	meta, err := dbjson.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, newBal, txType, description, meta); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1
	`, workspaceID, newBal, earned, spent); err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}

	return nil
}

// apply is the shared transactional path Credit + Debit funnel through.
// Opens its own transaction, delegates all SQL to applyTx, then commits.
// Existing callers (Credit / Debit) are unchanged.
func (s *LedgerStore) apply(
	ctx context.Context,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
	add bool,
) error {
	if s.pool == nil {
		// No DB — silently succeed so tests that skip the DB
		// path can still exercise higher-level mining logic.
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("mining: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, add); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreditTx adds `amount` LENS to workspaceID within a caller-supplied
// transaction. The caller owns Begin / Commit / Rollback — this lets
// multiple ledger credits (and other DB writes) share one atomic transaction.
func (s *LedgerStore) CreditTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: credit amount must be positive")
	}
	return s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, true)
}

// DebitTx removes `amount` LENS from workspaceID within a caller-supplied
// transaction. Returns ErrInsufficientBalance when the balance is too low.
// The caller owns Begin / Commit / Rollback.
func (s *LedgerStore) DebitTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	amount int64, // µLENS
	txType, description string,
	metadata map[string]interface{},
) error {
	if amount <= 0 {
		return errors.New("mining: debit amount must be positive")
	}
	return s.applyTx(ctx, tx, workspaceID, amount, txType, description, metadata, false)
}

// GetBalance returns 0.0 (not an error) for a workspace with no
// ledger history. That matches the spec — new workspaces are a
// supported case, not an exceptional one.
func (s *LedgerStore) GetBalance(ctx context.Context, workspaceID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT balance FROM lens_token_balances WHERE workspace_id = $1
	`, workspaceID)
	var b int64
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

// MinTransferAmount is the floor for Transfer, in µLENS (SEC-2). Spec-mandated
// — below this we risk dust attacks polluting the ledger.
const MinTransferAmount int64 = 1_000 // 0.001 LENS

// TypeTransfer / TypeBurn are the ledger row types for the
// Batch-3 economy primitives.
const (
	TypeTransferOut = "transfer_out"
	TypeTransferIn  = "transfer_in"
	TypeBurn        = "burn"
)

// TypePoolRoyalty tags a Pool-B royalty mint (Phase-2 Stage 2.1): a served
// cross-tenant pooled cache hit credits the contributing workspace
// s × avoided_COGS. Canonical home is here with the other ledger row types;
// internal/poolroyalty (the writer) aliases it. Counted in GetTotalSupply
// since Stage 2.2 — a royalty mint is LENS entering circulation.
const TypePoolRoyalty = "pool_royalty"

// Transfer atomically debits `from` and credits `to`. Both
// ledger rows + both balance updates happen inside one
// transaction so a partial failure can't drop or duplicate LENS.
func (s *LedgerStore) Transfer(
	ctx context.Context,
	fromWorkspace, toWorkspace string,
	amount int64, // µLENS
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

	// Impose a global lock order: always acquire the lexicographically
	// smaller workspace ID first, regardless of debit/credit direction.
	//
	// Without this, two concurrent opposite-direction transfers deadlock:
	//   Tx A: Transfer("ws_alice", "ws_bob") → locks ws_alice → waits for ws_bob
	//   Tx B: Transfer("ws_bob", "ws_alice") → locks ws_bob   → waits for ws_alice
	//
	// With a consistent global order both transactions always lock the same
	// workspace first, so one blocks and waits while the other completes —
	// no cycle, no deadlock.
	firstWS, secondWS := fromWorkspace, toWorkspace
	if firstWS > secondWS {
		firstWS, secondWS = secondWS, firstWS
	}

	// Acquire both locks before any writes.
	firstBal, firstEarned, firstSpent, err := readBalance(ctx, tx, firstWS)
	if err != nil {
		return err
	}
	secondBal, secondEarned, secondSpent, err := readBalance(ctx, tx, secondWS)
	if err != nil {
		return err
	}

	// Map locked balances back to from/to semantics.
	var fromBal, fromEarned, fromSpent int64
	var toBal, toEarned, toSpent int64
	if fromWorkspace == firstWS {
		fromBal, fromEarned, fromSpent = firstBal, firstEarned, firstSpent
		toBal, toEarned, toSpent = secondBal, secondEarned, secondSpent
	} else {
		fromBal, fromEarned, fromSpent = secondBal, secondEarned, secondSpent
		toBal, toEarned, toSpent = firstBal, firstEarned, firstSpent
	}

	// Debit `from`.
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
func (s *LedgerStore) Burn(ctx context.Context, workspaceID string, amount int64, description string) error {
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
// every credit-side ledger row that came from a mining track or the Pool-B
// royalty mint (Stage 2.2: pool_royalty counted so royalty LENS is honestly
// in supply). Explicit allow-list: transfers and marketplace_fee move
// existing LENS (not mints) and stay excluded; receipt_mine_provisional
// stays excluded per its own go-live treatment (PoVI preconditions).
func (s *LedgerStore) GetTotalSupply(ctx context.Context) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount), 0)
		FROM lens_token_ledger
		WHERE amount > 0 AND type IN ($1, $2, $3, $4, $5, $6)`,
		TypeCacheMine, TypeComputeMine, TypeEmbeddingMine,
		TypeAnnotationMine, TypePatternMine, TypePoolRoyalty)
	var n int64
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
func (s *LedgerStore) GetCirculatingSupply(ctx context.Context) (int64, error) {
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
	var burned int64
	if err := row.Scan(&burned); err != nil {
		return 0, fmt.Errorf("mining: burned: %w", err)
	}
	return total - burned, nil
}

// GetTotalBurned returns the cumulative LENS removed from supply — both plain
// burns (TypeBurn) AND slashed stake (TypeStakeSlash). Counting slashes keeps
// the economy-stats display (GetEconomyStats = total − burned) consistent with
// the slash-aware GetCirculatingSupply.
func (s *LedgerStore) GetTotalBurned(ctx context.Context) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(-amount), 0) FROM lens_token_ledger WHERE type IN ($1, $2)`,
		TypeBurn, TypeStakeSlash)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: total burned: %w", err)
	}
	return n, nil
}

// ─── tx helpers shared by Transfer + Burn ───────

// readBalance + writeBalance + insertLedgerRow factor out the SQL
// the Transfer / Burn flows share. They take a pgx.Tx so the
// caller controls transactional semantics.

func readBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, earned, spent int64, err error) {
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

func writeBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, earned, spent int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, earned, spent)
	if err != nil {
		return fmt.Errorf("mining: update balance: %w", err)
	}
	return nil
}

func insertLedgerRow(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter int64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("mining: marshal metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger
			(workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("mining: insert ledger row: %w", err)
	}
	return nil
}

// GetHistory returns the ledger entries for a workspace, newest
// first. Bounds: limit ≤ 200, offset ≥ 0.
// requesterMetaKey is the counterparty workspace id some mint writers stamp into
// a CONTRIBUTOR's ledger row (cache_mine; any pre-#155 pool-royalty held rows).
// It is FUNCTIONALLY read by CountCacheHitsBenefited via SQL on the STORED row,
// so it must stay stored — but it must never be echoed to the tenant in history.
const requesterMetaKey = "request_workspace_id"

// servedToPattern matches the trailing "served to <workspace>" leak shape — the
// cache_mine "cache hit (X) served to <requester>" description. Targeted (not a
// blind replace of the requester value, which could blunder on short/common
// substrings and wrongly couple the two vectors).
var servedToPattern = regexp.MustCompile(`served to \S+$`)

// maskHistoryEntry scrubs counterparty identity from a ledger entry BEFORE it is
// returned to a tenant (#145 family). Two INDEPENDENT vectors:
//   - metadata: drop request_workspace_id — GENERIC, fires on the key's presence
//     for any row type (defense in depth vs future leaky writers).
//   - description: neutralize a trailing "served to <workspace>".
//
// It mutates only the in-memory entry (a per-row value with a freshly-unmarshaled
// metadata map); the stored DB row and the SQL counter that reads
// request_workspace_id are provably untouched. Retroactive: masks historic rows
// on read, no backfill.
func maskHistoryEntry(e *LedgerEntry) {
	delete(e.Metadata, requesterMetaKey) // safe on nil / absent
	if servedToPattern.MatchString(e.Description) {
		e.Description = servedToPattern.ReplaceAllString(e.Description, "served to another workspace")
	}
}

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
		maskHistoryEntry(&e) // #145: scrub counterparty identity from the tenant echo
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─── CacheMiner ──────────────────────────────────

// CacheMiner is the LENS earner for the cache-contribution
// track. Wraps a LedgerStore + the cross-workspace toggle.
type CacheMiner struct {
	ledger         *LedgerStore
	crossWorkspace bool
	linkageEnabled bool          // Phase-0: owner-linkage self-deal guard (off by default; the wire-up enables it)
	holdbackWindow time.Duration // Phase-1 Item 1: the mint lands HELD, finalizes after this (default 72h)
}

// SetHoldbackWindow sets the held→spendable delay for cache mints (Phase-1 Item 1).
// Non-positive keeps the 72h default. The mint lands HELD and the finalize sweeper
// settles it after this window (revocable before then).
func (m *CacheMiner) SetHoldbackWindow(d time.Duration) {
	if d > 0 {
		m.holdbackWindow = d
	}
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
		holdbackWindow: 72 * time.Hour, // Phase-1 Item 1: default held window (mirrors pool-royalty)
	}
}

// SetOwnerLinkageCheck enables/disables the owner-linkage self-deal guard
// (Phase-0). Off by default (parity with the pool-royalty minter); the cache
// wire-up must enable it. When on, a cross-workspace cache hit between two
// workspaces the SAME operator controls is downgraded to the same-workspace rate
// (the 10× cross bonus is never minted to a self-deal).
func (m *CacheMiner) SetOwnerLinkageCheck(enabled bool) { m.linkageEnabled = enabled }

// ownerLinkedSQL is the owner-linkage signal used to detect cache self-dealing.
// TRUE iff the two workspaces share ANY captured card fingerprint (the carded
// one-operator-many-workspaces washer) OR ANY operator-declared owner_key (the
// ONLY signal that covers VOUCHED workspaces, which have no card — see
// migration 0088 and the Item A recon). EXISTS is false when neither signal is
// present (default-ALLOW on missing: an inconclusive check never blocks honest
// cross-actor reuse; the rate cap bounds yield regardless). Plain read, no lock.
const ownerLinkedSQL = `SELECT
    EXISTS (SELECT 1 FROM workspace_card_fingerprints a
            JOIN workspace_card_fingerprints b ON a.fingerprint_hash = b.fingerprint_hash
            WHERE a.workspace_id = $1 AND b.workspace_id = $2)
 OR EXISTS (SELECT 1 FROM workspace_owner_links a
            JOIN workspace_owner_links b ON a.owner_key = b.owner_key
            WHERE a.workspace_id = $1 AND b.workspace_id = $2)`

// areLinked reports whether owner and requester are the same operator (shared
// card fingerprint OR declared owner_key). A query error is surfaced fail-CLOSED
// by the caller (a linkage check we can't run must not pay the cross bonus).
func (m *CacheMiner) areLinked(ctx context.Context, owner, requester string) (bool, error) {
	var linked bool
	if err := m.ledger.pool.QueryRow(ctx, ownerLinkedSQL, owner, requester).Scan(&linked); err != nil {
		return false, fmt.Errorf("cache mining: owner-linkage check: %w", err)
	}
	return linked, nil
}

// RecordCacheHit credits the cache owner for serving a hit.
// hitType ∈ {"exact", "semantic"}. The two workspace IDs may be
// equal (workspace serving its own cache → the throttled tiny reward).
//
// requestID MUST be a SERVER-DERIVED work-product key (cache content hash) so
// the mint is idempotent on (requestID, owner); an empty requestID mints
// nothing (fail-closed). This track is dormant today (the proxy cache-hit path
// only increments the Prometheus counter, not this); the live wire-up supplies
// the id. The mint is gated on verified-to-earn via CreditOnce.
func (m *CacheMiner) RecordCacheHit(
	ctx context.Context,
	cacheOwnerWorkspace string,
	requestWorkspace string,
	hitType string,
	requestID string,
) error {
	if cacheOwnerWorkspace == "" {
		// No owner recorded — older cache entries from before
		// owner tracking landed. Skip (no one to credit).
		return nil
	}

	earning := m.earnFor(cacheOwnerWorkspace, requestWorkspace, hitType)
	if earning <= 0 {
		return nil
	}
	// Phase-0 owner-linkage self-deal guard: a cross-workspace rate paid between two
	// workspaces the SAME operator controls is self-dealing (the cache track had no
	// such guard while pool-royalty did). Downgrade a linked pair to the
	// same-workspace rate so the 10× cross bonus is never minted to a self-deal.
	// Covers CARDED (shared fingerprint) AND VOUCHED (declared owner_key) workspaces
	// — see areLinked / migration 0088. Fail-CLOSED: a linkage check we cannot run
	// must not pay the cross bonus.
	if m.linkageEnabled && earning > CacheHitSameWorkspace && cacheOwnerWorkspace != requestWorkspace {
		linked, err := m.areLinked(ctx, cacheOwnerWorkspace, requestWorkspace)
		if err != nil {
			return err
		}
		if linked {
			earning = CacheHitSameWorkspace
		}
	}

	meta := map[string]interface{}{
		"hit_type":             hitType,
		"request_workspace_id": requestWorkspace,
	}
	desc := fmt.Sprintf("cache hit (%s) served to %s", hitType, requestWorkspace)
	// Phase-1 Item 1: mint HELD (not spendable). CreditOnceHeld writes the uncounted
	// cache_mine_held row + a traffic_mint_holds claim; the finalize sweeper settles it
	// to the counted cache_mine after the window, and RevokeHeldTxAs can reverse it
	// before then (the clawback surface). Exactly-once on (requestID, owner, cache_mine).
	_, err := m.ledger.CreditOnceHeld(ctx, requestID, cacheOwnerWorkspace, earning, TypeCacheMine, desc, m.holdbackWindow, meta)
	return err
}

// earnFor encapsulates the rate-selection rules — same workspace
// gets the tiny reward; cross-workspace gets the bigger reward
// when sharing is enabled; semantic hits get the middle rate
// even cross-workspace.
func (m *CacheMiner) earnFor(owner, requester, hitType string) int64 {
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
	served, err := m.ledger.CountCacheHitsServed(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	benefited, err := m.ledger.CountCacheHitsBenefited(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	// EstimatedMonthly: lifetimeEarned across a guessed
	// onboarding-to-now window. Without a per-day breakdown we
	// project from "rough average across the workspace lifetime",
	// floored at 30 days so a brand-new workspace doesn't
	// extrapolate one big hit into a giant projection.
	// EstimatedMonthly is a projection (display-only) — compute in float from the
	// integer µLENS lifetime, then floor back to an integer µLENS count.
	var monthly int64
	if !snap.UpdatedAt.IsZero() {
		days := time.Since(snap.UpdatedAt).Hours()/24 + 1
		if days < 30 {
			days = 30
		}
		monthly = int64(float64(snap.LifetimeEarned) / days * 30)
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

const countServedSQL = `SELECT COUNT(*) FROM lens_token_ledger WHERE workspace_id = $1 AND type = 'cache_mine'`

// CountCacheHitsServed returns the number of cache-mine credits issued to
// workspaceID — the count of cache hits this workspace's entries served to any
// requester. Queries PG directly so the count is consistent across instances.
func (s *LedgerStore) CountCacheHitsServed(ctx context.Context, workspaceID string) (int, error) {
	if s.pool == nil {
		return 0, nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, countServedSQL, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: count served: %w", err)
	}
	return n, nil
}

const countBeneditedSQL = `SELECT COUNT(*) FROM lens_token_ledger WHERE type = 'cache_mine' AND metadata->>'request_workspace_id' = $1`

// CountCacheHitsBenefited returns the number of times workspaceID benefited
// from another workspace's cache entry. Reads the request_workspace_id field
// stored in ledger metadata. Cross-partition query — analytics only, not hot path.
func (s *LedgerStore) CountCacheHitsBenefited(ctx context.Context, workspaceID string) (int, error) {
	if s.pool == nil {
		return 0, nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, countBeneditedSQL, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("mining: count benefited: %w", err)
	}
	return n, nil
}

// Rates returns the public rate table in µLENS (SEC-2) — backs the
// /v1/tokens/rates endpoint. Values are integer µLENS per hit.
func Rates() map[string]int64 {
	return map[string]int64{
		"cache_hit_same":  CacheHitSameWorkspace,
		"cache_hit_cross": CacheHitCrossWorkspace,
		"semantic_hit":    SemanticCacheHit,
	}
}
