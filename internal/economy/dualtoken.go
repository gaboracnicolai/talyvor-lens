package economy

// dualtoken.go — the two-token split (Master Plan Upgrade 1).
//
// LENS is the volatile, tradeable, mined token (the existing
// one — role-clarified). LXC is a USD-pegged, one-way compute
// credit: 1 LXC = $0.10 of compute, minted by converting LENS
// at the admin-approved rate, spent on AI calls, and NEVER
// convertible back to LENS or fiat.
//
// Splitting the roles kills the arbitrage of a single token
// trying to be both a stablecoin and a speculative asset: the
// peg lives entirely in LXC (a fixed constant, never computed),
// while price discovery lives entirely in LENS.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbjson"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// ─── constants ───────────────────────────────────

const (
	// LXCUSDValue is the FIXED peg: 1 LXC always equals $0.10 of
	// compute credit. This is a constant — it is never computed,
	// derived, or adjusted. The peg is what makes LXC a stable
	// unit of account.
	LXCUSDValue = 0.10

	// ConversionSpread is the company margin / stability buffer
	// applied on top of the fair rate when minting LXC. 5%.
	ConversionSpread = 0.05

	// MaxRateChangePct caps how far an approved rate can move from
	// the previous approved rate in a single approval (±10%). Stops
	// a sudden backing-value swing from whipsawing the conversion
	// rate.
	MaxRateChangePct = 0.10

	// Phase1FloorRate is the conservative fixed floor for the
	// LENS-per-LXC rate until a live marketplace price exists.
	// TODO(phase2): replace this fixed floor with a floor derived
	// from the live marketplace mid-price once there is real
	// LENS price discovery on the order book.
	Phase1FloorRate = 1.0

	// MinConversionLXC is the smallest LXC amount a single
	// conversion can mint — blocks dust conversions.
	MinConversionLXC = 0.10

	// Ledger row types for the LXC + conversion paths.
	LXCTypeConvertFromLENS = "convert_from_lens"
	LXCTypeSpend           = "spend"
	LXCTypePurchase        = "purchase"
	LENSTypeConvertToLXC   = "convert_to_lxc"
)

// ─── errors ──────────────────────────────────────

var (
	ErrInsufficientLXC     = errors.New("economy: insufficient LXC balance")
	ErrConversionTooSmall  = fmt.Errorf("economy: conversion below minimum of %v LXC", MinConversionLXC)
	ErrInsufficientLENSFor = errors.New("economy: insufficient LENS balance for conversion")
)

// ─── types ───────────────────────────────────────

// RateComputation captures every intermediate value the rate
// engine used so an approved rate is fully reconstructable and
// the dashboard can show the math.
type RateComputation struct {
	FairRate     float64 `json:"fair_rate"`     // R_fair (before spread)
	AdminRate    float64 `json:"admin_rate"`    // R_admin (final, after spread + guards)
	BackingValue float64 `json:"backing_value"` // USD value backing 1 LENS
	Circulating  float64 `json:"circulating"`   // circulating LENS supply
	Spread       float64 `json:"spread"`        // policy spread applied
	PreviousRate float64 `json:"previous_rate"` // last approved rate
	Clamped      bool    `json:"clamped"`       // true if the ±band limited it
	Floored      bool    `json:"floored"`       // true if the Phase1 floor applied
}

// ConvertResult is what ConvertLENStoLXC returns.
type ConvertResult struct {
	LXCMinted      float64 `json:"lxc_minted"`
	LENSSpent      float64 `json:"lens_spent"`
	Rate           float64 `json:"rate"`
	NewLXCBalance  float64 `json:"new_lxc_balance"`
	NewLENSBalance float64 `json:"new_lens_balance"`
}

// LXCSnapshot mirrors a row of lxc_balances.
type LXCSnapshot struct {
	WorkspaceID    string  `json:"workspace_id"`
	Balance        float64 `json:"balance"`
	LifetimeMinted float64 `json:"lifetime_minted"`
	LifetimeSpent  float64 `json:"lifetime_spent"`
	USDValue       float64 `json:"usd_value"`
}

// ─── RateEngine ──────────────────────────────────

// RateEngine derives the LENS->LXC conversion rate from the
// backing value of mined work + the circulating LENS supply. The
// admin can only ever APPROVE the engine's output (clamped to a
// ±band and a floor) — there is no path to set an arbitrary rate.
type RateEngine struct {
	ledger *mining.LedgerStore
	pool   pgxDB
}

// NewRateEngine wraps a real pool.
func NewRateEngine(ledger *mining.LedgerStore, pool *pgxpool.Pool) *RateEngine {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newRateEngine(ledger, db)
}

func newRateEngine(ledger *mining.LedgerStore, db pgxDB) *RateEngine {
	return &RateEngine{ledger: ledger, pool: db}
}

// ComputeFairRate derives the rate from current supply + backing.
//
//	backing_value_per_LENS = total_USD_value_of_verified_work
//	                         / circulating_LENS_supply
//	R_fair  = LXCUSDValue / backing_value_per_LENS
//	R_admin = R_fair * (1 + ConversionSpread)
//	         then clamped to ±MaxRateChangePct of the previous
//	         approved rate, then floored at Phase1FloorRate.
//
// The total USD value of verified work is the all-time minted
// LENS (sum of mining-track credits) valued at the peg — each
// LENS was minted as the reward for compute work worth, at mint
// time, the peg's value. Circulating supply is minted minus
// burned, so as LENS is spent/burned the remaining supply is
// backed by proportionally more work and the fair rate falls.
func (e *RateEngine) ComputeFairRate(ctx context.Context) (RateComputation, error) {
	totalMinted, err := e.ledger.GetTotalSupply(ctx)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: total supply: %w", err)
	}
	circulating, err := e.ledger.GetCirculatingSupply(ctx)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: circulating supply: %w", err)
	}
	prev, err := e.CurrentRate(ctx)
	if err != nil {
		return RateComputation{}, err
	}

	comp := RateComputation{
		Spread:       ConversionSpread,
		Circulating:  circulating,
		PreviousRate: prev,
	}

	// Backing + fair rate. Guard against an empty economy (no LENS
	// minted yet or everything burned) — fall through to the floor.
	if totalMinted > 0 && circulating > 0 {
		totalWorkUSD := totalMinted * LXCUSDValue
		comp.BackingValue = totalWorkUSD / circulating
		comp.FairRate = LXCUSDValue / comp.BackingValue
	} else {
		comp.BackingValue = 0
		comp.FairRate = Phase1FloorRate
	}

	rAdmin := comp.FairRate * (1 + ConversionSpread)

	// Guard 1: clamp to ±MaxRateChangePct of the previous rate.
	if prev > 0 {
		hi := prev * (1 + MaxRateChangePct)
		lo := prev * (1 - MaxRateChangePct)
		if rAdmin > hi {
			rAdmin = hi
			comp.Clamped = true
		} else if rAdmin < lo {
			rAdmin = lo
			comp.Clamped = true
		}
	}

	// Guard 2: never below the Phase1 floor.
	if rAdmin < Phase1FloorRate {
		rAdmin = Phase1FloorRate
		comp.Floored = true
	}

	comp.AdminRate = roundTo(rAdmin, 6)
	return comp, nil
}

// CurrentRate returns the most recent approved rate from
// conversion_rate_history, or Phase1FloorRate when no rate has
// been approved yet.
func (e *RateEngine) CurrentRate(ctx context.Context) (float64, error) {
	if e.pool == nil {
		return Phase1FloorRate, nil
	}
	row := e.pool.QueryRow(ctx,
		`SELECT rate FROM conversion_rate_history ORDER BY created_at DESC LIMIT 1`)
	var rate float64
	if err := row.Scan(&rate); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Phase1FloorRate, nil
		}
		return 0, fmt.Errorf("economy: current rate: %w", err)
	}
	return rate, nil
}

// ApproveRate computes the fair rate, writes it to
// conversion_rate_history with all intermediate inputs, and
// returns the computation. This is the only way a rate changes —
// the admin approves the algorithm's output, never an arbitrary
// number.
func (e *RateEngine) ApproveRate(ctx context.Context, approvedBy string) (RateComputation, error) {
	comp, err := e.ComputeFairRate(ctx)
	if err != nil {
		return RateComputation{}, err
	}
	if e.pool == nil {
		return comp, nil
	}
	_, err = e.pool.Exec(ctx, `
		INSERT INTO conversion_rate_history
			(rate, fair_rate, backing_value, circulating, spread, previous_rate, approved_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		comp.AdminRate, comp.FairRate, comp.BackingValue, comp.Circulating,
		comp.Spread, comp.PreviousRate, approvedBy)
	if err != nil {
		return RateComputation{}, fmt.Errorf("economy: write rate history: %w", err)
	}
	return comp, nil
}

// RateHistoryEntry is one row of conversion_rate_history for the
// public history endpoint.
type RateHistoryEntry struct {
	Rate         float64   `json:"rate"`
	FairRate     float64   `json:"fair_rate"`
	BackingValue float64   `json:"backing_value"`
	Circulating  float64   `json:"circulating"`
	Spread       float64   `json:"spread"`
	PreviousRate float64   `json:"previous_rate"`
	ApprovedBy   string    `json:"approved_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// RateHistory returns recent rate approvals, newest first.
func (e *RateEngine) RateHistory(ctx context.Context, limit int) ([]RateHistoryEntry, error) {
	if e.pool == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := e.pool.Query(ctx, `
		SELECT rate, fair_rate, backing_value, circulating, spread, previous_rate, approved_by, created_at
		FROM conversion_rate_history ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("economy: rate history: %w", err)
	}
	defer rows.Close()
	var out []RateHistoryEntry
	for rows.Next() {
		var h RateHistoryEntry
		if err := rows.Scan(&h.Rate, &h.FairRate, &h.BackingValue, &h.Circulating,
			&h.Spread, &h.PreviousRate, &h.ApprovedBy, &h.CreatedAt); err != nil {
			return nil, fmt.Errorf("economy: scan rate history: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ─── DualTokenStore ──────────────────────────────

// DualTokenStore owns the LXC side + the LENS->LXC conversion.
// It holds a reference to the LENS LedgerStore for reads, but the
// conversion itself runs as a single inline transaction touching
// both tokens' tables so the debit + mint can never partially
// apply.
type DualTokenStore struct {
	lens   *mining.LedgerStore
	pool   pgxDB
	engine *RateEngine
}

// NewDualTokenStore wraps a real pool.
func NewDualTokenStore(lens *mining.LedgerStore, pool *pgxpool.Pool, engine *RateEngine) *DualTokenStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newDualTokenStore(lens, db, engine)
}

func newDualTokenStore(lens *mining.LedgerStore, db pgxDB, engine *RateEngine) *DualTokenStore {
	return &DualTokenStore{lens: lens, pool: db, engine: engine}
}

// ConvertLENStoLXC mints LXC by spending LENS at the current
// approved rate. ONE-WAY: there is deliberately no inverse method
// — LXC can never be converted back to LENS or to fiat.
//
// The whole operation runs in a single transaction: read+debit
// the LENS balance, read+credit the LXC balance, and append a row
// to each token's ledger. Any failure rolls the lot back.
func (s *DualTokenStore) ConvertLENStoLXC(ctx context.Context, workspaceID string, lxcAmount float64) (ConvertResult, error) {
	if lxcAmount < MinConversionLXC {
		return ConvertResult{}, ErrConversionTooSmall
	}
	rate, err := s.engine.CurrentRate(ctx)
	if err != nil {
		return ConvertResult{}, err
	}
	lensCost := roundTo(lxcAmount*rate, 6)

	if s.pool == nil {
		// No-DB path (unit tests of the pure arithmetic).
		return ConvertResult{
			LXCMinted: lxcAmount,
			LENSSpent: lensCost,
			Rate:      rate,
		}, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("economy: begin conversion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// ── debit LENS ──
	lensBal, lensEarned, lensSpent, err := readLENSBalance(ctx, tx, workspaceID)
	if err != nil {
		return ConvertResult{}, err
	}
	if lensBal < lensCost {
		return ConvertResult{}, ErrInsufficientLENSFor
	}
	newLENS := roundTo(lensBal-lensCost, 6)
	meta := map[string]interface{}{"lxc_minted": lxcAmount, "rate": rate}
	if err := insertLENSLedger(ctx, tx, workspaceID, -lensCost, newLENS,
		LENSTypeConvertToLXC, "converted to LXC", meta); err != nil {
		return ConvertResult{}, err
	}
	if err := writeLENSBalance(ctx, tx, workspaceID, newLENS, lensEarned, lensSpent+lensCost); err != nil {
		return ConvertResult{}, err
	}

	// ── credit LXC ──
	lxcBal, lxcMinted, lxcSpentTotal, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return ConvertResult{}, err
	}
	newLXC := roundTo(lxcBal+lxcAmount, 6)
	lxcMeta := map[string]interface{}{"lens_spent": lensCost, "rate": rate}
	if err := insertLXCLedger(ctx, tx, workspaceID, lxcAmount, newLXC,
		LXCTypeConvertFromLENS, "converted from LENS", lxcMeta); err != nil {
		return ConvertResult{}, err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newLXC, lxcMinted+lxcAmount, lxcSpentTotal); err != nil {
		return ConvertResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ConvertResult{}, fmt.Errorf("economy: commit conversion: %w", err)
	}
	metrics.ConvertedLXC(lxcAmount) // LXC minted via LENS→LXC conversion
	return ConvertResult{
		LXCMinted:      lxcAmount,
		LENSSpent:      lensCost,
		Rate:           rate,
		NewLXCBalance:  newLXC,
		NewLENSBalance: newLENS,
	}, nil
}

// SpendLXC debits the workspace's LXC balance — this is what the
// proxy bills against on each AI call. Fails with ErrInsufficientLXC
// when the balance can't cover the spend.
func (s *DualTokenStore) SpendLXC(ctx context.Context, workspaceID string, lxcAmount float64, description string) error {
	if lxcAmount <= 0 {
		return errors.New("economy: spend amount must be positive")
	}
	if s.pool == nil {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	bal, minted, spent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if bal < lxcAmount {
		return ErrInsufficientLXC
	}
	newBal := roundTo(bal-lxcAmount, 6)
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal,
		LXCTypeSpend, description, nil); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, spent+lxcAmount); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetLXCBalance returns the current LXC balance (0 for a fresh
// workspace — not an error).
func (s *DualTokenStore) GetLXCBalance(ctx context.Context, workspaceID string) (float64, error) {
	if s.pool == nil {
		return 0, nil
	}
	row := s.pool.QueryRow(ctx, `SELECT balance FROM lxc_balances WHERE workspace_id = $1`, workspaceID)
	var b float64
	if err := row.Scan(&b); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("economy: lxc balance: %w", err)
	}
	return b, nil
}

// GetLXCSnapshot returns the full LXC balance row + the USD value
// of the balance at the fixed peg.
func (s *DualTokenStore) GetLXCSnapshot(ctx context.Context, workspaceID string) (LXCSnapshot, error) {
	snap := LXCSnapshot{WorkspaceID: workspaceID}
	if s.pool == nil {
		return snap, nil
	}
	row := s.pool.QueryRow(ctx, `
		SELECT balance, lifetime_minted, lifetime_spent
		FROM lxc_balances WHERE workspace_id = $1`, workspaceID)
	if err := row.Scan(&snap.Balance, &snap.LifetimeMinted, &snap.LifetimeSpent); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return LXCSnapshot{}, fmt.Errorf("economy: lxc snapshot: %w", err)
		}
	}
	snap.USDValue = roundTo(snap.Balance*LXCUSDValue, 6)
	return snap, nil
}

// ─── tx-scoped SQL helpers ───────────────────────
// These mirror the LENS-side helpers in internal/mining but live
// here because the conversion tx spans both token schemas and
// the mining helpers are package-private.

func readLXCBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, minted, spent float64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lxc_balances (workspace_id, balance, lifetime_minted, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: ensure lxc balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_minted, lifetime_spent
		FROM lxc_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &minted, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: read lxc balance: %w", err)
	}
	return
}

func writeLXCBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, minted, spent float64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lxc_balances
		SET balance = $2, lifetime_minted = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, minted, spent)
	if err != nil {
		return fmt.Errorf("economy: update lxc balance: %w", err)
	}
	return nil
}

func insertLXCLedger(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter float64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("economy: marshal lxc ledger metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lxc_ledger (workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("economy: insert lxc ledger: %w", err)
	}
	return nil
}

// LENS-side tx helpers — the conversion debits LENS inside the
// same tx that mints LXC, so we re-implement the minimal SQL
// against lens_token_* here.

func readLENSBalance(ctx context.Context, tx pgx.Tx, workspaceID string) (bal, earned, spent float64, err error) {
	if _, err = tx.Exec(ctx, `
		INSERT INTO lens_token_balances (workspace_id, balance, lifetime_earned, lifetime_spent)
		VALUES ($1, 0, 0, 0) ON CONFLICT (workspace_id) DO NOTHING`, workspaceID); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: ensure lens balance row: %w", err)
	}
	row := tx.QueryRow(ctx, `
		SELECT balance, lifetime_earned, lifetime_spent
		FROM lens_token_balances WHERE workspace_id = $1 FOR UPDATE`, workspaceID)
	if err = row.Scan(&bal, &earned, &spent); err != nil {
		return 0, 0, 0, fmt.Errorf("economy: read lens balance: %w", err)
	}
	return
}

func writeLENSBalance(ctx context.Context, tx pgx.Tx, workspaceID string, bal, earned, spent float64) error {
	_, err := tx.Exec(ctx, `
		UPDATE lens_token_balances
		SET balance = $2, lifetime_earned = $3, lifetime_spent = $4, updated_at = NOW()
		WHERE workspace_id = $1`, workspaceID, bal, earned, spent)
	if err != nil {
		return fmt.Errorf("economy: update lens balance: %w", err)
	}
	return nil
}

func insertLENSLedger(ctx context.Context, tx pgx.Tx, workspaceID string, delta, balanceAfter float64,
	txType, description string, metadata map[string]interface{}) error {
	meta, err := dbjson.Marshal(metadata) // JSON text on both protocols (#133)
	if err != nil {
		return fmt.Errorf("economy: marshal lens ledger metadata: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO lens_token_ledger (workspace_id, amount, balance_after, type, description, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		workspaceID, delta, balanceAfter, txType, description, meta); err != nil {
		return fmt.Errorf("economy: insert lens ledger: %w", err)
	}
	return nil
}
