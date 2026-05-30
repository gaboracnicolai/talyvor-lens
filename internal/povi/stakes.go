package povi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/talyvor/lens/internal/metrics"
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
	Amount        float64     `json:"amount"` // current locked collateral
	Status        StakeStatus `json:"status"`
	SlashedAmount float64     `json:"slashed_amount"` // cumulative slashed (audit)
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

// StakeLedger is the atomic ledger lock primitive (mining.LedgerStore satisfies
// it). PoVI reuses the verb (lock/release/slash on locked_balance); the noun
// (the povi_stakes table + this manager) is PoVI-owned.
type StakeLedger interface {
	LockStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error
	ReleaseStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error
	SlashStake(ctx context.Context, workspaceID string, amount float64, metadata map[string]interface{}) error
}

// stakeStore persists Stake rows (pgStakeStore is the real impl; tests use an
// in-memory fake). Get returns (nil, nil) when absent.
type stakeStore interface {
	Get(ctx context.Context, nodeID string) (*Stake, error)
	Put(ctx context.Context, s Stake) error
	List(ctx context.Context) ([]Stake, error)
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
	minStake     float64
	unbondPeriod time.Duration
	now          func() time.Time
}

// NewStakeManager wires the store, the ledger lock primitive, the node→
// workspace lookup, the minimum stake, and the unbonding delay.
func NewStakeManager(store stakeStore, ledger StakeLedger, nodeWS NodeWorkspaceLookup, minStake float64, unbondPeriod time.Duration) *StakeManager {
	return &StakeManager{
		store:        store,
		ledger:       ledger,
		nodeWS:       nodeWS,
		minStake:     minStake,
		unbondPeriod: unbondPeriod,
		now:          time.Now,
	}
}

// MinStake / UnbondPeriod expose config for the status endpoint.
func (m *StakeManager) MinStake() float64           { return m.minStake }
func (m *StakeManager) UnbondPeriod() time.Duration { return m.unbondPeriod }

// Stake locks `amount` LENS as collateral for a node (topping up an existing
// active stake, or starting fresh). Insufficient balance → the ledger lock
// fails and nothing is recorded.
func (m *StakeManager) Stake(ctx context.Context, nodeID string, amount float64) (*Stake, error) {
	if amount <= 0 {
		return nil, errors.New("povi: stake amount must be positive")
	}
	ws, err := m.nodeWS(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("povi: resolve node workspace: %w", err)
	}
	existing, err := m.store.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	// Lock the additional collateral atomically first; if it fails (e.g.
	// insufficient balance) nothing is recorded.
	if err := m.ledger.LockStake(ctx, ws, amount, map[string]interface{}{"node_id": nodeID}); err != nil {
		return nil, err
	}

	now := m.now().UTC()
	st := Stake{NodeID: nodeID, WorkspaceID: ws, Amount: amount, Status: StakeActive, LockedAt: now, UpdatedAt: now}
	if existing != nil && existing.Status == StakeActive {
		// Top up an active stake — preserve its locked_at + cumulative slashed.
		st.Amount = existing.Amount + amount
		st.LockedAt = existing.LockedAt
		st.SlashedAmount = existing.SlashedAmount
	}
	if err := m.store.Put(ctx, st); err != nil {
		// Best-effort refund so locked LENS isn't stranded without a record.
		_ = m.ledger.ReleaseStake(ctx, ws, amount, map[string]interface{}{"node_id": nodeID, "refund": true})
		return nil, err
	}
	m.refreshGauges(ctx)
	return &st, nil
}

// Unbond begins withdrawal: active → unbonding, stamping unbond_at = now +
// unbondPeriod. The stake stays slashable throughout (anti-yank).
func (m *StakeManager) Unbond(ctx context.Context, nodeID string) error {
	st, err := m.store.Get(ctx, nodeID)
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
	if err := m.store.Put(ctx, *st); err != nil {
		return err
	}
	m.refreshGauges(ctx)
	return nil
}

// Release returns the collateral once the unbonding delay has elapsed: locked →
// available via the ledger, status → released.
func (m *StakeManager) Release(ctx context.Context, nodeID string) error {
	st, err := m.store.Get(ctx, nodeID)
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
		if err := m.ledger.ReleaseStake(ctx, st.WorkspaceID, st.Amount, map[string]interface{}{"node_id": nodeID}); err != nil {
			return err
		}
	}
	st.Amount = 0
	st.Status = StakeReleased
	st.UpdatedAt = m.now().UTC()
	if err := m.store.Put(ctx, *st); err != nil {
		return err
	}
	m.refreshGauges(ctx)
	return nil
}

// Slash burns `fraction` of a node's collateral (the mechanism Part 3 triggers
// when a challenge fails). Slashable while active OR unbonding. Returns the
// amount slashed. After slashing, if the remaining stake < min the node is
// minting-ineligible until re-staked; a full slash marks the stake slashed.
func (m *StakeManager) Slash(ctx context.Context, nodeID string, fraction float64, reason string) (float64, error) {
	if fraction <= 0 || fraction > 1 {
		return 0, errors.New("povi: slash fraction must be in (0,1]")
	}
	st, err := m.store.Get(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	if st == nil {
		return 0, ErrNoStake
	}
	if !st.Slashable() {
		return 0, ErrNotSlashable
	}
	slashAmt := st.Amount * fraction
	if err := m.ledger.SlashStake(ctx, st.WorkspaceID, slashAmt, map[string]interface{}{"node_id": nodeID, "reason": reason}); err != nil {
		return 0, err
	}
	st.Amount -= slashAmt
	st.SlashedAmount += slashAmt
	st.UpdatedAt = m.now().UTC()
	if st.Amount <= 1e-9 {
		st.Amount = 0
		st.Status = StakeSlashed
	}
	if err := m.store.Put(ctx, *st); err != nil {
		return 0, err
	}
	metrics.POVISlash(slashAmt)
	m.refreshGauges(ctx)
	return slashAmt, nil
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
	TotalLocked    float64 `json:"total_locked"`
	EligibleNodes  int     `json:"eligible_nodes"`
	ActiveNodes    int     `json:"active_nodes"`
	UnbondingNodes int     `json:"unbonding_nodes"`
	MinStake       float64 `json:"min_stake"`
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
	metrics.SetPOVIStakeLocked(status.TotalLocked)
	metrics.SetPOVINodesStaked(float64(status.EligibleNodes))
}
