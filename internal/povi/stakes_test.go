package povi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ── test doubles ──

type ledgerCall struct {
	workspaceID string
	amount      int64
}

type fakeLedger struct {
	mu                       sync.Mutex
	locks, releases, slashes []ledgerCall
	lockErr                  error
}

func (f *fakeLedger) LockStake(_ context.Context, ws string, amount int64, _ map[string]interface{}) error {
	if f.lockErr != nil {
		return f.lockErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.locks = append(f.locks, ledgerCall{ws, amount})
	return nil
}
func (f *fakeLedger) ReleaseStake(_ context.Context, ws string, amount int64, _ map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, ledgerCall{ws, amount})
	return nil
}
func (f *fakeLedger) SlashStake(_ context.Context, ws string, amount int64, _ map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slashes = append(f.slashes, ledgerCall{ws, amount})
	return nil
}

// Tx variants delegate to the non-Tx methods (no real transaction in tests).
func (f *fakeLedger) LockStakeTx(_ context.Context, _ pgx.Tx, ws string, amount int64, m map[string]interface{}) error {
	return f.LockStake(context.Background(), ws, amount, m)
}
func (f *fakeLedger) ReleaseStakeTx(_ context.Context, _ pgx.Tx, ws string, amount int64, m map[string]interface{}) error {
	return f.ReleaseStake(context.Background(), ws, amount, m)
}
func (f *fakeLedger) SlashStakeTx(_ context.Context, _ pgx.Tx, ws string, amount int64, m map[string]interface{}) error {
	return f.SlashStake(context.Background(), ws, amount, m)
}

type memStore struct {
	mu sync.Mutex
	m  map[string]Stake
}

func newMemStore() *memStore { return &memStore{m: map[string]Stake{}} }
func (s *memStore) Get(_ context.Context, nodeID string) (*Stake, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[nodeID]
	if !ok {
		return nil, nil
	}
	cp := st
	return &cp, nil
}
func (s *memStore) Put(_ context.Context, st Stake) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[st.NodeID] = st
	return nil
}
func (s *memStore) List(_ context.Context) ([]Stake, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Stake
	for _, st := range s.m {
		out = append(out, st)
	}
	return out, nil
}

// Tx variants delegate to the non-Tx methods (no real transaction in tests).
func (s *memStore) GetTx(ctx context.Context, _ pgx.Tx, nodeID string) (*Stake, error) {
	return s.Get(ctx, nodeID)
}
func (s *memStore) PutTx(ctx context.Context, _ pgx.Tx, st Stake) error {
	return s.Put(ctx, st)
}

func fixedWS(ws string) NodeWorkspaceLookup {
	return func(_ context.Context, _ string) (string, error) { return ws, nil }
}

const testMin = int64(50)

func newTestManager(t *testing.T) (*StakeManager, *fakeLedger, *memStore) {
	t.Helper()
	led := &fakeLedger{}
	store := newMemStore()
	m := NewStakeManager(store, led, fixedWS("ws-op"), testMin, 7*24*time.Hour, nil)
	return m, led, store
}

// ── tests ──

// Locking stake calls the ledger lock op for the node's workspace and records
// an active stake.
func TestStake_LocksLedgerAndRecords(t *testing.T) {
	m, led, _ := newTestManager(t)
	st, err := m.Stake(context.Background(), "node-1", 100)
	if err != nil {
		t.Fatalf("Stake: %v", err)
	}
	if st.Amount != 100 || st.Status != StakeActive || st.WorkspaceID != "ws-op" {
		t.Errorf("stake = %+v", st)
	}
	if len(led.locks) != 1 || led.locks[0].amount != 100 || led.locks[0].workspaceID != "ws-op" {
		t.Errorf("ledger locks = %+v", led.locks)
	}
}

// Insufficient balance (ledger lock fails) → no stake recorded.
func TestStake_InsufficientBalanceFails(t *testing.T) {
	m, led, store := newTestManager(t)
	led.lockErr = errInsufficient
	if _, err := m.Stake(context.Background(), "node-1", 100); err == nil {
		t.Fatal("expected failure when ledger lock fails")
	}
	if got, _ := store.Get(context.Background(), "node-1"); got != nil {
		t.Error("no stake should be recorded when the lock failed")
	}
}

// Stake ≥ min → minting-eligible; below min → not.
func TestEligibility_MinThreshold(t *testing.T) {
	m, _, _ := newTestManager(t)
	if _, err := m.Stake(context.Background(), "rich", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stake(context.Background(), "poor", 49); err != nil {
		t.Fatal(err)
	}
	if !m.IsEligible(context.Background(), "rich") {
		t.Error("stake == min should be eligible")
	}
	if m.IsEligible(context.Background(), "poor") {
		t.Error("stake < min should NOT be eligible")
	}
	// An unstaked node is not eligible (but may still serve — see processor).
	if m.IsEligible(context.Background(), "unknown") {
		t.Error("unstaked node must not be eligible")
	}
}

// Unbonding: can't release before unbond_at; can after; release returns the
// collateral via the ledger and leaves the stake released.
func TestUnbond_DelayEnforced(t *testing.T) {
	m, led, _ := newTestManager(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }
	if _, err := m.Stake(context.Background(), "node-1", 100); err != nil {
		t.Fatal(err)
	}
	if err := m.Unbond(context.Background(), "node-1"); err != nil {
		t.Fatalf("Unbond: %v", err)
	}
	// Unbonding node is NOT minting-eligible (it's withdrawing).
	if m.IsEligible(context.Background(), "node-1") {
		t.Error("unbonding node must not be minting-eligible")
	}
	// Before unbond_at → release rejected.
	m.now = func() time.Time { return base.Add(6 * 24 * time.Hour) }
	if err := m.Release(context.Background(), "node-1"); !errors.Is(err, ErrUnbondPending) {
		t.Fatalf("release before unbond_at should fail with ErrUnbondPending, got %v", err)
	}
	if len(led.releases) != 0 {
		t.Error("no ledger release should occur before unbond_at")
	}
	// After unbond_at → release succeeds.
	m.now = func() time.Time { return base.Add(8 * 24 * time.Hour) }
	if err := m.Release(context.Background(), "node-1"); err != nil {
		t.Fatalf("Release after delay: %v", err)
	}
	if len(led.releases) != 1 || led.releases[0].amount != 100 {
		t.Errorf("expected one ledger release of 100, got %+v", led.releases)
	}
}

// THE ANTI-YANK PROPERTY: stake is slashable while active AND while unbonding —
// a node can't cheat then yank its stake out before a challenge slashes it.
func TestSlash_DuringUnbonding(t *testing.T) {
	m, led, _ := newTestManager(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }
	if _, err := m.Stake(context.Background(), "node-1", 100); err != nil {
		t.Fatal(err)
	}
	if err := m.Unbond(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	// Mid-unbond (before the delay elapses) a slash must still land.
	m.now = func() time.Time { return base.Add(2 * 24 * time.Hour) }
	slashed, err := m.Slash(context.Background(), "node-1", 0.5, "failed challenge")
	if err != nil {
		t.Fatalf("Slash during unbonding must succeed (anti-yank): %v", err)
	}
	if slashed != 50 {
		t.Errorf("slashed = %v, want 50 (half of 100)", slashed)
	}
	if len(led.slashes) != 1 || led.slashes[0].amount != 50 {
		t.Errorf("ledger slash = %+v", led.slashes)
	}
	st, _ := m.Get(context.Background(), "node-1")
	if st.Amount != 50 || st.SlashedAmount != 50 || st.Status != StakeUnbonding {
		t.Errorf("post-slash stake = %+v (should remain unbonding, amount 50)", st)
	}
}

// Partial slash flips eligibility when remaining < min; full slash marks the
// stake slashed.
func TestSlash_PartialAndEligibilityFlip(t *testing.T) {
	m, _, _ := newTestManager(t)
	if _, err := m.Stake(context.Background(), "node-1", 100); err != nil {
		t.Fatal(err)
	}
	if !m.IsEligible(context.Background(), "node-1") {
		t.Fatal("should start eligible")
	}
	// Slash 60% → 40 remaining < 50 min → ineligible, but still active.
	if _, err := m.Slash(context.Background(), "node-1", 0.6, "x"); err != nil {
		t.Fatal(err)
	}
	if m.IsEligible(context.Background(), "node-1") {
		t.Error("remaining (40) < min (50) → must be ineligible")
	}
	st, _ := m.Get(context.Background(), "node-1")
	if st.Status != StakeActive || st.Amount != 40 {
		t.Errorf("partial slash should keep active with amount 40, got %+v", st)
	}
	// Re-stake to top back up over min → eligible again.
	if _, err := m.Stake(context.Background(), "node-1", 20); err != nil {
		t.Fatal(err)
	}
	if !m.IsEligible(context.Background(), "node-1") {
		t.Error("re-staked to 60 ≥ 50 → should be eligible again")
	}
	// Full slash → slashed status, zero amount.
	if _, err := m.Slash(context.Background(), "node-1", 1.0, "x"); err != nil {
		t.Fatal(err)
	}
	st, _ = m.Get(context.Background(), "node-1")
	if st.Status != StakeSlashed || st.Amount != 0 {
		t.Errorf("full slash → slashed/0, got %+v", st)
	}
}

// A released stake is not slashable (it's already gone).
func TestSlash_NotSlashableAfterRelease(t *testing.T) {
	m, _, _ := newTestManager(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }
	if _, err := m.Stake(context.Background(), "node-1", 100); err != nil {
		t.Fatal(err)
	}
	_ = m.Unbond(context.Background(), "node-1")
	m.now = func() time.Time { return base.Add(8 * 24 * time.Hour) }
	_ = m.Release(context.Background(), "node-1")
	if _, err := m.Slash(context.Background(), "node-1", 0.5, "x"); !errors.Is(err, ErrNotSlashable) {
		t.Errorf("released stake must not be slashable, got %v", err)
	}
}

// Concurrent stake/unbond/eligibility must be race-free (run under -race).
func TestStakeManager_Concurrent(t *testing.T) {
	m, _, _ := newTestManager(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "node-" + string(rune('a'+i))
			_, _ = m.Stake(context.Background(), id, 100)
			_ = m.IsEligible(context.Background(), id)
			_ = m.Unbond(context.Background(), id)
		}()
	}
	wg.Wait()
}

var errInsufficient = errors.New("insufficient")
