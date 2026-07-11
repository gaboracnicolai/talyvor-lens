package poolroyalty

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// fakeFinalizer records FinalizeHeldTx calls (the real
// *mining.LedgerStore.FinalizeHeldTx matches the signature).
type finalizeCall struct {
	workspaceID string
	amount      int64
}

type fakeFinalizer struct {
	calls []finalizeCall
	err   error
}

func (f *fakeFinalizer) FinalizeHeldTx(_ context.Context, _ pgx.Tx, ws string, amount int64, _ string, _ map[string]interface{}) error {
	f.calls = append(f.calls, finalizeCall{workspaceID: ws, amount: amount})
	return f.err
}

func expectSweep(pool pgxmock.PgxPoolIface, rows *pgxmock.Rows) {
	pool.ExpectQuery(`FROM pool_royalty_mints`).WillReturnRows(rows)
}

// THE FINALIZE PATH: a due held row is settled in its OWN tx — CAS first
// (claims the row before any balance is touched), then held->spendable, then
// commit. The CAS is the double-finalize guard: RowsAffected==0 means another
// sweeper already settled it.
func TestSweeperRunOnce_FinalizesDueRows(t *testing.T) {
	pool := newMockPool(t)
	fin := &fakeFinalizer{}
	s := NewFinalizeSweeper(pool, fin, "pool_royalty_mints")

	expectSweep(pool, pgxmock.NewRows([]string{"request_id", "contributor_workspace_id", "minted_amount"}).
		AddRow("req-a", "wsA", micro(1.0)).
		AddRow("req-b", "wsC", micro(2.5)))

	for _, req := range []string{"req-a", "req-b"} {
		pool.ExpectBegin()
		pool.ExpectExec(`UPDATE pool_royalty_mints SET status = 'final'`).
			WithArgs(req).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		pool.ExpectCommit()
	}

	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("finalized=%d want 2", n)
	}
	if len(fin.calls) != 2 || fin.calls[0].workspaceID != "wsA" || fin.calls[0].amount != micro(1.0) ||
		fin.calls[1].workspaceID != "wsC" || fin.calls[1].amount != micro(2.5) {
		t.Errorf("finalize calls = %+v", fin.calls)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// CAS LOST (HA failover overlap): another sweeper already settled the row —
// RowsAffected 0 -> rollback and skip BEFORE touching any balance. Double-
// finalize impossible, same guarantee shape as povi_challenges' UNIQUE.
func TestSweeperRunOnce_CASLost_SkipsWithoutBalanceTouch(t *testing.T) {
	pool := newMockPool(t)
	fin := &fakeFinalizer{}
	s := NewFinalizeSweeper(pool, fin, "pool_royalty_mints")

	expectSweep(pool, pgxmock.NewRows([]string{"request_id", "contributor_workspace_id", "minted_amount"}).
		AddRow("req-a", "wsA", micro(1.0)))
	pool.ExpectBegin()
	pool.ExpectExec(`UPDATE pool_royalty_mints SET status = 'final'`).
		WithArgs("req-a").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0)) // another sweeper won
	pool.ExpectRollback()

	n, err := s.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("CAS-lost row must be skipped; n=%d err=%v", n, err)
	}
	if len(fin.calls) != 0 {
		t.Errorf("balance must NOT be touched on CAS loss; calls=%+v", fin.calls)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// PER-ROW ERROR ISOLATION: a failing settle rolls ITS row back (claim CAS +
// balance together — the row stays 'held' for the next tick) and the sweep
// continues with the remaining rows.
func TestSweeperRunOnce_RowErrorIsolated(t *testing.T) {
	pool := newMockPool(t)
	fin := &fakeFinalizer{err: errors.New("ledger down")}
	s := NewFinalizeSweeper(pool, fin, "pool_royalty_mints")

	expectSweep(pool, pgxmock.NewRows([]string{"request_id", "contributor_workspace_id", "minted_amount"}).
		AddRow("req-a", "wsA", micro(1.0)).
		AddRow("req-b", "wsC", micro(2.5)))
	for _, req := range []string{"req-a", "req-b"} {
		pool.ExpectBegin()
		pool.ExpectExec(`UPDATE pool_royalty_mints SET status = 'final'`).
			WithArgs(req).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		pool.ExpectRollback() // finalizer errors -> rollback; row stays held
	}

	n, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("per-row errors must not fail the sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("finalized=%d want 0 (both rows errored)", n)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Empty sweep + nil-safety.
func TestSweeperRunOnce_EmptyAndNilSafe(t *testing.T) {
	pool := newMockPool(t)
	s := NewFinalizeSweeper(pool, &fakeFinalizer{}, "pool_royalty_mints")
	expectSweep(pool, pgxmock.NewRows([]string{"request_id", "contributor_workspace_id", "minted_amount"}))
	if n, err := s.RunOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("empty sweep: n=%d err=%v", n, err)
	}

	var nilS *FinalizeSweeper
	if n, err := nilS.RunOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("nil sweeper must no-op; n=%d err=%v", n, err)
	}
}
