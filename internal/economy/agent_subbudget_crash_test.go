package economy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── synthetic mid-transaction fault injection (TEST-ONLY, no production seam) ──────────────────────────────
// faultyPool wraps a real pgxDB and, on the tx it hands out, forces any Exec whose SQL contains failOnSQL to
// return an error — simulating a crash / connection-drop MID-TRANSACTION. This is stricter than the
// natural-rejection atomicity test: it catches a future refactor that commits the claim in its own tx before
// the debit (which the ceiling-rejection test would miss). Injected purely via a wrapped pool assigned to the
// unexported DualTokenStore.pool field (in-package) — SpendLXCForAgent is unchanged.

type faultyPool struct {
	pgxDB
	failOnSQL string
}

func (f *faultyPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.pgxDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &faultyTx{Tx: tx, failOnSQL: f.failOnSQL}, nil
}

type faultyTx struct {
	pgx.Tx
	failOnSQL string
}

func (t *faultyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, t.failOnSQL) {
		return pgconn.CommandTag{}, errors.New("injected mid-tx fault (simulated crash between claim and commit)")
	}
	return t.Tx.Exec(ctx, sql, args...)
}

// (proof 3, synthetic) MID-TX CRASH: a fault AFTER the claim INSERT and BEFORE commit (here, the balance
// UPDATE in writeLXCBalance — past the claim, before tx.Commit) rolls the WHOLE tx back: no orphan claim, no
// debit, balance + spent unchanged — AND the request_id is NOT burned, so a legitimate retry SUCCEEDS.
func TestAgentSpend_SyntheticMidTxCrash_NoOrphanClaim(t *testing.T) {
	s := agentHarness(t) // real store + tables + TRUNCATE
	ctx := context.Background()
	fund(t, s, "wsX", 100*uLXC)

	// The faulty store shares the SAME real pool but faults on the balance UPDATE — which executes AFTER the
	// claim INSERT + ceiling check, BEFORE tx.Commit. Simulates a crash mid-debit.
	sf := &DualTokenStore{pool: &faultyPool{pgxDB: s.pool, failOnSQL: "UPDATE lxc_balances"}}

	err := sf.SpendLXCForAgent(ctx, "keyX", "wsX", "req-crash", 10*uLXC, "t")
	if err == nil {
		t.Fatal("the injected mid-tx fault must surface as an error (the debit failed)")
	}

	// (1)+(2)+(3) the tx rolled back — no orphan claim, balance unchanged, spent unchanged.
	bal, spent, claims := agentState(t, s, "wsX", "keyX")
	if bal != 100*uLXC || spent != 0 || claims != 0 {
		t.Fatalf("mid-tx crash must roll back EVERYTHING: bal=%d spent=%d claims=%d, want 100/0/0 (x1e6)", bal, spent, claims)
	}
	var claimRow int
	_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM lxc_spend_claims WHERE request_id='req-crash'`).Scan(&claimRow)
	if claimRow != 0 {
		t.Fatalf("ORPHAN CLAIM: request_id 'req-crash' must have NO row after the rolled-back tx, got %d", claimRow)
	}

	// (4) THE KEY ASSERTION: the request_id was NOT burned — a legitimate retry (real, non-faulty pool)
	// SUCCEEDS and debits exactly once. An orphan claim would have made this wrongly no-op.
	if err := s.SpendLXCForAgent(ctx, "keyX", "wsX", "req-crash", 10*uLXC, "t"); err != nil {
		t.Fatalf("retry of the SAME request_id after a rolled-back crash must SUCCEED (id not burned), got %v", err)
	}
	if bal, spent, claims := agentState(t, s, "wsX", "keyX"); bal != 90*uLXC || spent != 10*uLXC || claims != 1 {
		t.Fatalf("post-crash retry must debit EXACTLY once: bal=%d spent=%d claims=%d, want 90/10/1 (x1e6)", bal, spent, claims)
	}
}
