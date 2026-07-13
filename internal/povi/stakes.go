package povi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/mining"
)

// Node-registration staking (PoVI Part 2 / Upgrade 6). A node must lock LENS
// collateral to become MINTING-ELIGIBLE; that collateral is slashable (Part 3),
// so getting caught cheating costs real value instead of a free re-registration.
//
// Staking does NOT gate registration or serving — an unstaked node still
// registers and serves; it is simply not minting-eligible (its receipts are
// recorded as ineligible). This module owns the node-staking CONCEPT
// (collateral + unbonding + slashing); it reuses the shared ledger's atomic
// lock primitive (StakeLedger) for the available↔locked↔burned mechanics.

// StakeStatus is the lifecycle of a node's collateral.
type StakeStatus string

const (
	StakeActive    StakeStatus = "active"    // locked + minting-eligible (if ≥ min)
	StakeUnbonding StakeStatus = "unbonding" // withdrawing; NOT eligible, STILL slashable
	StakeReleased  StakeStatus = "released"  // collateral returned; terminal
	StakeSlashed   StakeStatus = "slashed"   // fully forfeited; terminal
)

// Stake is one node's collateral record.
type Stake struct {
	NodeID        string      `json:"node_id"`
	WorkspaceID   string      `json:"workspace_id"`
	Amount        int64       `json:"amount_ulens"` // current locked collateral, µLENS (SEC-2)
	Status        StakeStatus `json:"status"`
	SlashedAmount int64       `json:"slashed_amount_ulens"` // cumulative slashed (audit), µLENS
	LockedAt      time.Time   `json:"locked_at"`
	UnbondAt      *time.Time  `json:"unbond_at,omitempty"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Slashable reports whether this collateral can still be slashed. The anti-yank
// property: slashable while active OR unbonding (so a node can't cheat then
// instantly withdraw before a challenge slashes it). Released/slashed stakes
// are not slashable.
func (s Stake) Slashable() bool {
	return (s.Status == StakeActive || s.Status == StakeUnbonding) && s.Amount > 0
}

// txBeginner is the minimal DB interface StakeManager needs to open a
// transaction for wrapping ledger + store ops atomically.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// StakeLedger is the atomic ledger lock primitive (mining.LedgerStore satisfies
// it). Each operation has a self-contained variant (its own tx) and a Tx variant
// (runs inside a caller-supplied transaction for cross-table atomicity).
type StakeLedger interface {
	LockStake(ctx context.Context, workspaceID string, amount int64, metadata map[string]interface{}) error
	ReleaseStake(ctx context.Context, workspaceID string, amount int64, metadata map[string]interface{}) error
	SlashStake(ctx context.Context, workspaceID string, amount int64, metadata map[string]interface{}) error
	LockStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
	ReleaseStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
	SlashStakeTx(ctx context.Context, tx pgx.Tx, workspaceID string, amount int64, metadata map[string]interface{}) error
}

// stakeStore persists Stake rows. Get/Put are the non-transactional variants
// (test/legacy path); GetTx/PutTx run inside an external transaction.
type stakeStore interface {
	Get(ctx context.Context, nodeID string) (*Stake, error)
	Put(ctx context.Context, s Stake) error
	List(ctx context.Context) ([]Stake, error)
	GetTx(ctx context.Context, tx pgx.Tx, nodeID string) (*Stake, error)
	PutTx(ctx context.Context, tx pgx.Tx, s Stake) error
}

// NodeWorkspaceLookup resolves a node's operator workspace (node→workspace is
// 1:1 in inference_nodes).
type NodeWorkspaceLookup func(ctx context.Context, nodeID string) (string, error)

var (
	ErrNoStake       = errors.New("povi: node has no stake")
	ErrNotActive     = errors.New("povi: stake is not active")
	ErrUnbondPending = errors.New("povi: unbonding period has not elapsed")
	ErrNotSlashable  = errors.New("povi: stake is not slashable")
)

// StakeManager orchestrates node staking: it drives the ledger lock primitive
// and records the lifecycle in the stake store.
type StakeManager struct {
	store        stakeStore
	ledger       StakeLedger
	nodeWS       NodeWorkspaceLookup
	minStake     int64
	unbondPeriod time.Duration
	now          func() time.Time
	db           txBeginner // nil in tests → non-transactional path
	// repSink (P1 #9, optional) appends a negative reputation event IN the slash tx — skin-in-the-
	// game beyond stake (invariant 4). nil ⇒ no-op (byte-identical); wired only when the
	// reputation-bonded-minting flag is on.
	repSink SlashReputationSink
}

// SlashReputationSink appends a workspace-keyed reputation event on the slash tx (atomic with the
// stake burn). Satisfied by *mining.ReputationStore.RecordEventTx. Wired only when the P1 #9 flag is on.
type SlashReputationSink interface {
	RecordEventTx(ctx context.Context, tx pgx.Tx, workspaceID, kind, idemKey string, delta float64, reason any) error
}

// SlashReputationDelta is the reputation hit a slash applies (P1 #9, invariant 4): a node slashed for
// a failed challenge earns less on every future bonded mint until it rebuilds R.
const SlashReputationDelta = -0.10

// SetReputationSink wires the optional slash→reputation emitter (P1 #9). nil ⇒ Slash appends no
// reputation event (byte-identical). A setter so NewStakeManager's signature stays put.
func (m *StakeManager) SetReputationSink(sink SlashReputationSink) { m.repSink = sink }

// NewStakeManager wires the store, the ledger lock primitive, the node→workspace
// lookup, the minimum stake, the unbonding delay, and the DB pool used to open
// a single transaction per stake operation (pass nil in tests).
func NewStakeManager(store stakeStore, ledger StakeLedger, nodeWS NodeWorkspaceLookup, minStake int64, unbondPeriod time.Duration, db txBeginner) *StakeManager {
	return &StakeManager{
		store:        store,
		ledger:       ledger,
		nodeWS:       nodeWS,
		minStake:     minStake,
		unbondPeriod: unbondPeriod,
		now:          time.Now,
		db:           db,
	}
}

// runStakeOp wraps fn in a single transaction guarded by a Postgres advisory
// lock on nodeID, serializing concurrent ops for the same node. Falls back to
// the non-transactional path when m.db is nil (test doubles).
func (m *StakeManager) runStakeOp(ctx context.Context, nodeID string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if m.db == nil {
		return fn(ctx, nil)
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("povi: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, nodeID); err != nil {
		return fmt.Errorf("povi: advisory lock: %w", err)
	}
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// storeGet dispatches to GetTx when inside a transaction, Get otherwise.
func (m *StakeManager) storeGet(ctx context.Context, tx pgx.Tx, nodeID string) (*Stake, error) {
	if tx == nil {
		return m.store.Get(ctx, nodeID)
	}
	return m.store.GetTx(ctx, tx, nodeID)
}

// storePut dispatches to PutTx when inside a transaction, Put otherwise.
func (m *StakeManager) storePut(ctx context.Context, tx pgx.Tx, st Stake) error {
	if tx == nil {
		return m.store.Put(ctx, st)
	}
	return m.store.PutTx(ctx, tx, st)
}

// ledgerLock/Release/Slash dispatch to the Tx variants when inside a
// transaction, the non-Tx variants otherwise.
func (m *StakeManager) ledgerLockStake(ctx context.Context, tx pgx.Tx, ws string, amount int64, meta map[string]interface{}) error {
	if tx == nil {
		return m.ledger.LockStake(ctx, ws, amount, meta)
	}
	return m.ledger.LockStakeTx(ctx, tx, ws, amount, meta)
}
func (m *StakeManager) ledgerReleaseStake(ctx context.Context, tx pgx.Tx, ws string, amount int64, meta map[string]interface{}) error {
	if tx == nil {
		return m.ledger.ReleaseStake(ctx, ws, amount, meta)
	}
	return m.ledger.ReleaseStakeTx(ctx, tx, ws, amount, meta)
}
func (m *StakeManager) ledgerSlashStake(ctx context.Context, tx pgx.Tx, ws string, amount int64, meta map[string]interface{}) error {
	if tx == nil {
		return m.ledger.SlashStake(ctx, ws, amount, meta)
	}
	return m.ledger.SlashStakeTx(ctx, tx, ws, amount, meta)
}

// MinStake / UnbondPeriod expose config for the status endpoint.
func (m *StakeManager) MinStake() int64             { return m.minStake }
func (m *StakeManager) UnbondPeriod() time.Duration { return m.unbondPeriod }

// WorkspaceForNode resolves a node's operator workspace (the same lookup the
// stake ops use internally). Exposed so the request path can enforce ownership
// BEFORE a stake/unbond/release: a caller may only act on its own node (#146
// Phase 3). Returns an error when the node is unknown.
func (m *StakeManager) WorkspaceForNode(ctx context.Context, nodeID string) (string, error) {
	return m.nodeWS(ctx, nodeID)
}

// Stake locks `amount` LENS as collateral for a node (topping up an existing
// active stake, or starting fresh). All reads and writes run in a single
// transaction so concurrent stakes for the same node serialize correctly.
func (m *StakeManager) Stake(ctx context.Context, nodeID string, amount int64) (*Stake, error) {
	if amount <= 0 {
		return nil, errors.New("povi: stake amount must be positive")
	}
	ws, err := m.nodeWS(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("povi: resolve node workspace: %w", err)
	}

	var result *Stake
	opErr := m.runStakeOp(ctx, nodeID, func(ctx context.Context, tx pgx.Tx) error {
		existing, err := m.storeGet(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if err := m.ledgerLockStake(ctx, tx, ws, amount, map[string]interface{}{"node_id": nodeID}); err != nil {
			return err
		}
		now := m.now().UTC()
		st := Stake{NodeID: nodeID, WorkspaceID: ws, Amount: amount, Status: StakeActive, LockedAt: now, UpdatedAt: now}
		if existing != nil && existing.Status == StakeActive {
			st.Amount = existing.Amount + amount
			st.LockedAt = existing.LockedAt
			st.SlashedAmount = existing.SlashedAmount
		}
		if err := m.storePut(ctx, tx, st); err != nil {
			if tx == nil {
				// Non-transactional path: attempt to return the locked collateral so
				// LENS isn't stranded without a povi_stakes record.
				_ = m.ledger.ReleaseStake(ctx, ws, amount, map[string]interface{}{"node_id": nodeID, "refund": true})
			}
			return err
		}
		result = &st
		return nil
	})
	if opErr != nil {
		return nil, opErr
	}
	m.refreshGauges(ctx)
	return result, nil
}

// Unbond begins withdrawal: active → unbonding, stamping unbond_at = now +
// unbondPeriod. The stake stays slashable throughout (anti-yank).
func (m *StakeManager) Unbond(ctx context.Context, nodeID string) error {
	err := m.runStakeOp(ctx, nodeID, func(ctx context.Context, tx pgx.Tx) error {
		st, err := m.storeGet(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if st == nil {
			return ErrNoStake
		}
		if st.Status != StakeActive {
			return ErrNotActive
		}
		now := m.now().UTC()
		unbondAt := now.Add(m.unbondPeriod)
		st.Status = StakeUnbonding
		st.UnbondAt = &unbondAt
		st.UpdatedAt = now
		return m.storePut(ctx, tx, *st)
	})
	if err != nil {
		return err
	}
	m.refreshGauges(ctx)
	return nil
}

// Release returns the collateral once the unbonding delay has elapsed: locked →
// available via the ledger, status → released. The ledger op and the
// status update happen in one transaction.
func (m *StakeManager) Release(ctx context.Context, nodeID string) error {
	err := m.runStakeOp(ctx, nodeID, func(ctx context.Context, tx pgx.Tx) error {
		st, err := m.storeGet(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if st == nil {
			return ErrNoStake
		}
		if st.Status != StakeUnbonding {
			return ErrNotActive
		}
		if st.UnbondAt == nil || m.now().UTC().Before(*st.UnbondAt) {
			return ErrUnbondPending
		}
		if st.Amount > 0 {
			if err := m.ledgerReleaseStake(ctx, tx, st.WorkspaceID, st.Amount, map[string]interface{}{"node_id": nodeID}); err != nil {
				return err
			}
		}
		st.Amount = 0
		st.Status = StakeReleased
		st.UpdatedAt = m.now().UTC()
		return m.storePut(ctx, tx, *st)
	})
	if err != nil {
		return err
	}
	m.refreshGauges(ctx)
	return nil
}

// Slash burns `fraction` of a node's collateral (the mechanism Part 3 triggers
// when a challenge fails). Slashable while active OR unbonding. The ledger op
// and the stake-record update run in one transaction so concurrent slashes
// can't double-burn or leave the record inconsistent. Returns the amount slashed.
func (m *StakeManager) Slash(ctx context.Context, nodeID string, fraction float64, reason string) (int64, error) {
	if fraction <= 0 || fraction > 1 {
		return 0, errors.New("povi: slash fraction must be in (0,1]")
	}
	var slashed int64 // µLENS
	opErr := m.runStakeOp(ctx, nodeID, func(ctx context.Context, tx pgx.Tx) error {
		st, err := m.storeGet(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		if st == nil {
			return ErrNoStake
		}
		if !st.Slashable() {
			return ErrNotSlashable
		}
		// SEC-2 site #5 (slash). st.Amount is conserved µLENS collateral; fraction
		// is a Tier-2 float in (0,1]. A slash is a BURN/DEBIT against a misbehaving
		// node, so it rounds UP (MulCeil) — the protocol never under-burns a
		// sub-µLENS; the extra sub-unit is destroyed (removed from supply). For
		// fraction≤1, ceil(amount×fraction) ≤ amount, so this never over-burns the
		// locked balance. The burned remainder is fully accounted (it leaves supply).
		slashAmt := mining.MulCeil(st.Amount, fraction)
		if err := m.ledgerSlashStake(ctx, tx, st.WorkspaceID, slashAmt, map[string]interface{}{"node_id": nodeID, "reason": reason}); err != nil {
			return err
		}
		st.Amount -= slashAmt
		st.SlashedAmount += slashAmt
		st.UpdatedAt = m.now().UTC()
		// SEC-2: exact integer zero check (replaces the float-epsilon `<= 1e-9`).
		if st.Amount == 0 {
			st.Status = StakeSlashed
		}
		if err := m.storePut(ctx, tx, *st); err != nil {
			return err
		}
		// P1 #9 invariant 4: lower the node's reputation IN the slash tx (atomic with the burn). The
		// idem_key is the challenge reason ("challenge_<result>:<requestID>", unique per attempt) so a
		// retried slash is idempotent. tx==nil on the non-transactional test path → skip (best-effort).
		if m.repSink != nil && tx != nil {
			if err := m.repSink.RecordEventTx(ctx, tx, st.WorkspaceID, "slash", reason, SlashReputationDelta,
				map[string]interface{}{"node_id": nodeID, "reason": reason, "slashed": slashAmt}); err != nil {
				return err
			}
		}
		slashed = slashAmt
		return nil
	})
	if opErr != nil {
		return 0, opErr
	}
	metrics.POVISlash(mining.MicroToFloat(slashed))
	m.refreshGauges(ctx)
	return slashed, nil
}

// IsEligible reports whether a node is minting-eligible: an ACTIVE stake at or
// above the minimum. Unbonding/released/slashed/unknown nodes are not eligible.
func (m *StakeManager) IsEligible(ctx context.Context, nodeID string) bool {
	st, err := m.store.Get(ctx, nodeID)
	if err != nil || st == nil {
		return false
	}
	return st.Status == StakeActive && st.Amount >= m.minStake
}

// Get returns a node's stake (nil if none).
func (m *StakeManager) Get(ctx context.Context, nodeID string) (*Stake, error) {
	return m.store.Get(ctx, nodeID)
}

// List returns all stakes (for the dashboard / status).
func (m *StakeManager) List(ctx context.Context) ([]Stake, error) { return m.store.List(ctx) }

// StakingStatus aggregates totals for the /v1/povi/staking/status endpoint.
type StakingStatus struct {
	TotalLocked    int64   `json:"total_locked_ulens"` // µLENS (SEC-2)
	EligibleNodes  int     `json:"eligible_nodes"`
	ActiveNodes    int     `json:"active_nodes"`
	UnbondingNodes int     `json:"unbonding_nodes"`
	MinStake       int64   `json:"min_stake_ulens"` // µLENS (SEC-2)
	UnbondSeconds  float64 `json:"unbond_period_seconds"`
}

// Status computes the aggregate staking view.
func (m *StakeManager) Status(ctx context.Context) (StakingStatus, error) {
	stakes, err := m.store.List(ctx)
	if err != nil {
		return StakingStatus{}, err
	}
	out := StakingStatus{MinStake: m.minStake, UnbondSeconds: m.unbondPeriod.Seconds()}
	for _, s := range stakes {
		switch s.Status {
		case StakeActive:
			out.TotalLocked += s.Amount
			out.ActiveNodes++
			if s.Amount >= m.minStake {
				out.EligibleNodes++
			}
		case StakeUnbonding:
			out.TotalLocked += s.Amount
			out.UnbondingNodes++
		}
	}
	return out, nil
}

// refreshGauges recomputes the locked-LENS + staked-node gauges from the store.
// Off the hot path (stake ops are administrative), so a List per mutation is
// fine. Best-effort: a gauge refresh failure never fails the stake op.
func (m *StakeManager) refreshGauges(ctx context.Context) {
	status, err := m.Status(ctx)
	if err != nil {
		return
	}
	metrics.SetPOVIStakeLocked(mining.MicroToFloat(status.TotalLocked))
	metrics.SetPOVINodesStaked(float64(status.EligibleNodes))
}
