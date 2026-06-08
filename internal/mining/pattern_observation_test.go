package mining

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// THE MAKE-OR-BREAK: RecordPatternObservation is CAPTURE-ONLY and structurally
// MINT-FREE. Proof by construction: the PatternMiner is built with a NIL
// ledger. Since the method never references m.ledger, a nil ledger is fine; if
// it ever called m.ledger.Credit, Credit would nil-deref (*LedgerStore) and
// PANIC. So "no panic + persists" == "never touches the ledger" == cannot mint.
func TestRecordPatternObservation_CannotMint_NilLedger(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	// nil ledger — the structural mint-free guarantee.
	m := NewPatternMiner(nil, mock)

	// Capture issues the opt-in-gated conditional INSERT (no RETURNING, no
	// Credit query). The WHERE EXISTS gates the write on consent in SQL.
	mock.ExpectExec("INSERT INTO routing_patterns").
		WithArgs("wsA", "chat", "gpt-4o", "openai", "0-1k", 0.9, "fast", 1.0, 1.0, 1).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	p := RoutingPattern{
		FeatureCategory: "chat", ModelUsed: "gpt-4o", ProviderUsed: "openai",
		InputTokenRange: "0-1k", OutputQuality: 0.9, LatencyBucket: "fast",
		CacheHitRate: 1.0, SuccessRate: 1.0, SampleCount: 1,
	}
	// Must NOT panic (nil ledger) and must NOT error.
	if err := m.RecordPatternObservation(context.Background(), "wsA", p); err != nil {
		t.Fatalf("RecordPatternObservation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (capture must issue exactly the INSERT, no Credit): %v", err)
	}
}

// Nil pool / nil miner are inert no-ops (no panic).
func TestRecordPatternObservation_NilSafe(t *testing.T) {
	if err := NewPatternMiner(nil, nil).RecordPatternObservation(context.Background(), "ws", RoutingPattern{}); err != nil {
		t.Errorf("nil pool must no-op; got %v", err)
	}
	var nilM *PatternMiner
	if err := nilM.RecordPatternObservation(context.Background(), "ws", RoutingPattern{}); err != nil {
		t.Errorf("nil miner must no-op; got %v", err)
	}
}
