package mining

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockAnnotator(t *testing.T) (*AnnotationMiner, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return NewAnnotationMiner(newLedgerStore(mock), mock), mock
}

// ─── PII detection ───────────────────────────────

func TestContainsPII_DetectsEmail(t *testing.T) {
	if !ContainsPII("reach me at alice@example.com please") {
		t.Fatal("expected email to be detected")
	}
	if !ContainsPII("Contact: BOB.SMITH@example.org") {
		t.Fatal("expected uppercase email to be detected")
	}
}

func TestContainsPII_DetectsPhone(t *testing.T) {
	for _, s := range []string{
		"call 415-555-1234",
		"phone: 415.555.1234",
		"4155551234 is the number",
	} {
		if !ContainsPII(s) {
			t.Fatalf("expected phone detected in %q", s)
		}
	}
}

func TestContainsPII_DetectsCreditCard(t *testing.T) {
	for _, s := range []string{
		"card 4111 1111 1111 1111",
		"4111-1111-1111-1111 is the number",
	} {
		if !ContainsPII(s) {
			t.Fatalf("expected card detected in %q", s)
		}
	}
}

func TestContainsPII_CleanText(t *testing.T) {
	clean := []string{
		"The quick brown fox jumps over the lazy dog.",
		"function helloWorld() { return 42 }",
		"",
		"Just plain text without any PII patterns at all.",
	}
	for _, s := range clean {
		if ContainsPII(s) {
			t.Fatalf("expected no PII in %q", s)
		}
	}
}

// ─── CreateTask ──────────────────────────────────

func TestCreateTask_RejectsPII(t *testing.T) {
	miner, _ := newMockAnnotator(t)
	_, err := miner.CreateTask(context.Background(), "ws_src", "hash",
		"clean response", "Contact: alice@example.com")
	if !errors.Is(err, ErrResponseContainsPII) {
		t.Fatalf("expected ErrResponseContainsPII, got %v", err)
	}
}

func TestCreateTask_HappyPath(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectQuery("INSERT INTO annotation_tasks").
		WithArgs("ws_src", "hash123", "resp A", "resp B", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("t1", time.Now()))
	task, err := miner.CreateTask(context.Background(), "ws_src", "hash123",
		"resp A", "resp B")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.ID != "t1" || task.ExpiresAt.IsZero() {
		t.Fatalf("unexpected task: %+v", task)
	}
}

// ─── SubmitAnnotation ────────────────────────────

func TestSubmitAnnotation_RejectsExpired(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t_old").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_src", time.Now().Add(-time.Hour)))
	mock.ExpectRollback()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t_old", AnnotatorID: "ws_anno", Decision: "a_better",
	})
	if !errors.Is(err, ErrTaskExpired) {
		t.Fatalf("expected ErrTaskExpired, got %v", err)
	}
}

func TestSubmitAnnotation_RejectsSelfAnnotation(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t_self").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_same", time.Now().Add(time.Hour)))
	mock.ExpectRollback()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t_self", AnnotatorID: "ws_same", Decision: "a_better",
	})
	if !errors.Is(err, ErrSelfAnnotation) {
		t.Fatalf("expected ErrSelfAnnotation, got %v", err)
	}
}

func TestSubmitAnnotation_RejectsInvalidDecision(t *testing.T) {
	// Decision validation is pre-DB; no Begin needed.
	miner, _ := newMockAnnotator(t)
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t1", AnnotatorID: "ws_a", Decision: "unsure",
	})
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("expected ErrInvalidDecision, got %v", err)
	}
}

func TestSubmitAnnotation_InsufficientStake(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t1").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_src", time.Now().Add(time.Hour)))
	// Stake lookup with FOR UPDATE returns 5 LENS < 10 requirement.
	mock.ExpectQuery("SELECT staked FROM annotator_stakes").
		WithArgs("ws_anno").
		WillReturnRows(pgxmock.NewRows([]string{"staked"}).AddRow(5.0))
	mock.ExpectRollback()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t1", AnnotatorID: "ws_anno", Decision: "a_better", Confidence: 4,
	})
	if !errors.Is(err, ErrInsufficientStake) {
		t.Fatalf("expected ErrInsufficientStake, got %v", err)
	}
}

func TestSubmitAnnotation_RejectsDuplicate(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t_dup").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_src", time.Now().Add(time.Hour)))
	mock.ExpectQuery("SELECT staked FROM annotator_stakes").
		WithArgs("ws_anno").
		WillReturnRows(pgxmock.NewRows([]string{"staked"}).AddRow(20.0))
	// Confidence: 0 is clamped to 1 by the miner.
	mock.ExpectExec("INSERT INTO annotations").
		WithArgs("t_dup", "ws_anno", "a_better", 1, 0).
		WillReturnError(errors.New("ERROR: duplicate key value violates unique constraint (SQLSTATE 23505)"))
	mock.ExpectRollback()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t_dup", AnnotatorID: "ws_anno", Decision: "a_better",
	})
	if !errors.Is(err, ErrDuplicateAnnotation) {
		t.Fatalf("expected ErrDuplicateAnnotation, got %v", err)
	}
}

func TestSubmitAnnotation_CreditsBaseReward(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t_credit").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_src", time.Now().Add(time.Hour)))
	// FOR UPDATE stake read inside the outer tx.
	mock.ExpectQuery("SELECT staked FROM annotator_stakes").
		WithArgs("ws_anno").
		WillReturnRows(pgxmock.NewRows([]string{"staked"}).AddRow(20.0))
	mock.ExpectExec("INSERT INTO annotations").
		WithArgs("t_credit", "ws_anno", "a_better", 4, 5000).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Agreement query inside the same tx — only the just-inserted row visible.
	mock.ExpectQuery("SELECT decision FROM annotations WHERE task_id").
		WithArgs("t_credit").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).AddRow("a_better"))
	// CreditTx runs inside the outer tx (no nested Begin/Commit).
	expectApplyTx(mock, "ws_anno", 0, 0, 0, 0.100, 0.100, 0.100, 0)
	mock.ExpectCommit()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t_credit", AnnotatorID: "ws_anno",
		Decision: "a_better", Confidence: 4, TimeSpentMs: 5000,
	})
	if err != nil {
		t.Fatalf("SubmitAnnotation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSubmitAnnotation_PaysAgreementBonus(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT source_workspace, expires_at").
		WithArgs("t_bonus").
		WillReturnRows(pgxmock.NewRows([]string{"source_workspace", "expires_at"}).
			AddRow("ws_src", time.Now().Add(time.Hour)))
	mock.ExpectQuery("SELECT staked FROM annotator_stakes").
		WithArgs("ws_anno").
		WillReturnRows(pgxmock.NewRows([]string{"staked"}).AddRow(20.0))
	mock.ExpectExec("INSERT INTO annotations").
		WithArgs("t_bonus", "ws_anno", "a_better", 5, 3000).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Agreement query — 3 existing "a_better" + this insert → 4 total.
	// After subtracting the just-inserted row: 3 matches out of 3 others = 100%.
	mock.ExpectQuery("SELECT decision FROM annotations WHERE task_id").
		WithArgs("t_bonus").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).
			AddRow("a_better").
			AddRow("a_better").
			AddRow("a_better").
			AddRow("a_better"))
	// Base 0.100 + bonus 0.050 = 0.150 via CreditTx (no nested Begin/Commit).
	expectApplyTx(mock, "ws_anno", 0, 0, 0, 0.150, 0.150, 0.150, 0)
	mock.ExpectCommit()
	err := miner.SubmitAnnotation(context.Background(), Annotation{
		TaskID: "t_bonus", AnnotatorID: "ws_anno",
		Decision: "a_better", Confidence: 5, TimeSpentMs: 3000,
	})
	if err != nil {
		t.Fatalf("SubmitAnnotation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── CheckAgreement ──────────────────────────────

func TestCheckAgreement_NoOtherAnnotations(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectQuery("SELECT decision FROM annotations WHERE task_id").
		WithArgs("t_alone").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).AddRow("a_better"))
	agreement, err := miner.CheckAgreement(context.Background(), "t_alone", "a_better")
	if err != nil {
		t.Fatalf("CheckAgreement: %v", err)
	}
	if agreement != 0 {
		t.Fatalf("solo annotation should report 0 agreement, got %f", agreement)
	}
}

func TestCheckAgreement_PartialMajority(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	mock.ExpectQuery("SELECT decision FROM annotations WHERE task_id").
		WithArgs("t_split").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).
			AddRow("a_better"). // matches new decision
			AddRow("a_better").
			AddRow("b_better").
			AddRow("a_better")) // including the just-inserted one
	// 3 a_better total, minus the just-inserted = 2; others = 4 - 1 = 3.
	// agreement = 2/3 ≈ 0.667
	agreement, err := miner.CheckAgreement(context.Background(), "t_split", "a_better")
	if err != nil {
		t.Fatalf("CheckAgreement: %v", err)
	}
	expected := 2.0 / 3.0
	if diff := agreement - expected; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("expected %f, got %f", expected, agreement)
	}
}

// ─── Stake ───────────────────────────────────────

// expectApplyTx sets up the four SQL steps that LedgerStore.applyTx
// executes on a caller-supplied tx (no Begin/Commit — those belong to
// the caller). Used when testing functions that call DebitTx/CreditTx.
func expectApplyTx(
	mock pgxmock.PgxPoolIface,
	workspaceID string,
	startingBalance, startingEarned, startingSpent float64,
	delta, expectedBalance, expectedEarned, expectedSpent float64,
) {
	mock.ExpectExec("INSERT INTO lens_token_balances").
		WithArgs(workspaceID).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT balance, lifetime_earned, lifetime_spent").
		WithArgs(workspaceID).
		WillReturnRows(pgxmock.NewRows([]string{"balance", "lifetime_earned", "lifetime_spent"}).
			AddRow(startingBalance, startingEarned, startingSpent))
	mock.ExpectExec("INSERT INTO lens_token_ledger").
		WithArgs(workspaceID, delta, expectedBalance, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE lens_token_balances").
		WithArgs(workspaceID, expectedBalance, expectedEarned, expectedSpent).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func TestStake_DeductsFromBalance(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	// Stake() owns the transaction; DebitTx runs inside it without Begin/Commit.
	mock.ExpectBegin()
	expectApplyTx(mock, "ws_stake", 15.0, 15.0, 0, -10.0, 5.0, 15.0, 10.0)
	mock.ExpectExec("INSERT INTO annotator_stakes").
		WithArgs("ws_stake", 10.0).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	if err := miner.Stake(context.Background(), "ws_stake", 10.0); err != nil {
		t.Fatalf("Stake: %v", err)
	}
}

func TestStake_RejectsZero(t *testing.T) {
	miner, _ := newMockAnnotator(t)
	if err := miner.Stake(context.Background(), "ws", 0); err == nil {
		t.Fatal("expected error for zero stake")
	}
}

// ─── GetAnnotatorStats ───────────────────────────

func TestGetAnnotatorStats_ReturnsTotals(t *testing.T) {
	miner, mock := newMockAnnotator(t)
	// Annotation count.
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM annotations WHERE annotator_id").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(42))
	// Earnings.
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(amount\\), 0\\)\\s+FROM lens_token_ledger").
		WithArgs("ws_stats", TypeAnnotationMine).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(4.20))
	// Stake.
	mock.ExpectQuery("SELECT staked FROM annotator_stakes").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{"staked"}).AddRow(25.0))
	// Agreement query.
	mock.ExpectQuery("WITH mine AS").
		WithArgs("ws_stats").
		WillReturnRows(pgxmock.NewRows([]string{"avg"}).AddRow(0.82))
	stats, err := miner.GetAnnotatorStats(context.Background(), "ws_stats")
	if err != nil {
		t.Fatalf("GetAnnotatorStats: %v", err)
	}
	if stats.Annotations != 42 || stats.EarnedTokens != 4.20 || stats.StakedTokens != 25.0 {
		t.Fatalf("unexpected totals: %+v", stats)
	}
	if stats.Agreement < 0.81 || stats.Agreement > 0.83 {
		t.Fatalf("expected agreement ~0.82, got %f", stats.Agreement)
	}
}

// ─── AnnotationRates ─────────────────────────────

func TestAnnotationRates(t *testing.T) {
	r := AnnotationRates()
	for _, k := range []string{
		"base_reward", "high_agreement_bonus", "stake_requirement",
		"reputation_decay_daily", "high_agreement_threshold",
	} {
		if _, ok := r[k]; !ok {
			t.Fatalf("missing rate key %q", k)
		}
	}
	if r["base_reward"] != AnnotationBaseReward {
		t.Fatal("base_reward value drift")
	}
}
