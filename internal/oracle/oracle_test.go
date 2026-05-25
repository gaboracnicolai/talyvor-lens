package oracle

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/lens/internal/mining"
)

func newMockOracle(t *testing.T, sampleRate float64) (*Oracle, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	ledger := mining.NewLedgerStoreForTesting(mock)
	annotator := mining.NewAnnotationMiner(ledger, mock)
	return newOracle(annotator, ledger, mock, sampleRate), mock
}

// ─── sampling ────────────────────────────────────

func TestCreateTaskFromRequest_Samples(t *testing.T) {
	// Force a 100% sample rate so the CreateTask call happens.
	oracle, mock := newMockOracle(t, 1.0)
	mock.ExpectQuery("INSERT INTO annotation_tasks").
		WithArgs("ws_src", "hash", "responseA", "responseB", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at"}).
			AddRow("t1", time.Now()))
	if err := oracle.CreateTaskFromRequest(context.Background(),
		"req-1", "hash", "responseA", "responseB", "ws_src"); err != nil {
		t.Fatalf("CreateTaskFromRequest: %v", err)
	}
}

func TestCreateTaskFromRequest_RespectsZeroSampleRate(t *testing.T) {
	oracle, _ := newMockOracle(t, 0)
	// No expectations programmed — must not touch DB.
	if err := oracle.CreateTaskFromRequest(context.Background(),
		"req-x", "hash", "respA", "respB", "ws_src"); err != nil {
		t.Fatalf("CreateTaskFromRequest: %v", err)
	}
}

func TestCreateTaskFromRequest_DeterministicSampling(t *testing.T) {
	oracle, _ := newMockOracle(t, 0.01)
	// At 1% sample rate, a deterministic hash → same answer
	// every time. We don't care which side of the gate we land
	// on, just that repeated calls match.
	first := oracle.shouldSample("stable-request-id")
	for i := 0; i < 10; i++ {
		if oracle.shouldSample("stable-request-id") != first {
			t.Fatalf("sampling not deterministic on call %d", i)
		}
	}
}

func TestCreateTaskFromRequest_ApproximatesSampleRate(t *testing.T) {
	oracle, _ := newMockOracle(t, 0.20)
	// Across many random IDs, the sampler should hit roughly
	// the configured rate. Wide tolerance because the FNV hash
	// + 10000-bucket modulo has some variance at small N.
	hits := 0
	for i := 0; i < 1000; i++ {
		if oracle.shouldSample("req-" + strconv.Itoa(i)) {
			hits++
		}
	}
	if hits < 150 || hits > 250 {
		t.Fatalf("expected ~200 hits at 20%% sample rate, got %d", hits)
	}
}

// ─── GetOracleStats ──────────────────────────────

func TestGetOracleStats_ReturnsCounts(t *testing.T) {
	oracle, mock := newMockOracle(t, 0.01)
	// pending
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM annotation_tasks WHERE expires_at").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(7))
	// completed
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM annotations$").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(123))
	// active oracles
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM annotator_stakes WHERE staked").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(5))
	// avg agreement
	mock.ExpectQuery("WITH per_task AS").
		WillReturnRows(pgxmock.NewRows([]string{"avg"}).AddRow(0.85))
	// tokens distributed
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(amount\\), 0\\)").
		WithArgs(mining.TypeAnnotationMine).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(12.34))

	stats, err := oracle.GetOracleStats(context.Background())
	if err != nil {
		t.Fatalf("GetOracleStats: %v", err)
	}
	if stats.PendingTasks != 7 || stats.CompletedTasks != 123 {
		t.Fatalf("unexpected task counts: %+v", stats)
	}
	if stats.ActiveOracles != 5 {
		t.Fatalf("unexpected active count: %+v", stats)
	}
	if stats.AvgAgreement < 0.84 || stats.AvgAgreement > 0.86 {
		t.Fatalf("unexpected agreement: %f", stats.AvgAgreement)
	}
	if stats.TokensDistributed != 12.34 {
		t.Fatalf("unexpected tokens distributed: %f", stats.TokensDistributed)
	}
}
