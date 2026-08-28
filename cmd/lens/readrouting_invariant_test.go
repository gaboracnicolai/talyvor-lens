package main

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talyvor/lens/internal/anomaly"
	"github.com/talyvor/lens/internal/costanomaly"
	"github.com/talyvor/lens/internal/distillattrib"
	"github.com/talyvor/lens/internal/forecast"
	"github.com/talyvor/lens/internal/keel"
)

// The read-replica wiring is the security boundary (U8/U9): money/authz/tx
// constructors must PHYSICALLY NEVER receive the replica pool. These guards
// turn "verified by hand in the wiring table" into "enforced at test time".

func mainGoSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReadReplicaWiring_ExactlySixAnalyticsReaders — exactly the six
// recon-confirmed analytics readers are routed via dbrouting.ReadPool; no more,
// no fewer. A change here means the replica wiring set moved and must be
// re-reviewed against the invariant table. (Grew 4→6 with U25 Keel: the two
// keel readers below were consciously reviewed as read-only analytics.)
//
// ⚠ COUNTED BY strings.Count OVER THE FILE UNTIL #517, SO A COMMENT COULD RESTORE A
// READER THAT HAD BEEN REMOVED: dropping costanomaly from the replica and leaving the
// old wiring behind as a commented line put the count back at 6 AND satisfied the
// per-reader presence check, and the "the wiring set moved, re-review it" tripwire did
// not fire. Now a real dbrouting.ReadPool CALL is counted, and each named reader must
// really wrap one.
func TestReadReplicaWiring_ExactlySixAnalyticsReaders(t *testing.T) {
	src := []byte(mainGoSource(t))
	scan, err := scanReplicaWiring("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if n := scan.readPoolCalls; n != 6 {
		t.Fatalf("expected EXACTLY 6 analytics readers routed via %s, found %d — the replica wiring set changed; re-review the invariant table", readPoolFunc, n)
	}
	for _, reader := range []string{
		"forecast.NewStore",
		"costanomaly.NewStore",
		"anomaly.New",
		"distillattrib.NewReader",
		// Keel (U25) drift attribution — this identical call appears TWICE in main.go
		// (the leader-elected drift-sweep corpus reader ~L576, and the requireAdmin
		// findings read handler ~L1300), which is why the count above is 6, not 5.
		// Both are READ-ONLY analytics over the consented corpus (routing_patterns)
		// + keel_findings and are replica-lag-tolerant; the keel_findings WRITE goes
		// to the PRIMARY via FindingsWriter(pool), never through the replica.
		"keel.NewReader",
	} {
		ok, err := scan.wraps(reader, readPoolFunc, src, "main.go")
		if err != nil {
			t.Fatalf("parse main.go: %v", err)
		}
		if !ok {
			t.Errorf("missing expected analytics→replica wiring: %s(%s(...))", reader, readPoolFunc)
		}
	}
}

// TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica — THE invariant: no
// money/authz/tx constructor receives the replica pool. A careless future edit
// (e.g. billing.New(dbrouting.ReadPool(...))) trips this.
//
// ⚠ THIS ASKED THE QUESTION OF A LINE UNTIL #517 AND A LINE BREAK DEFEATED IT. Measured
// by rewriting `keyStore := auth.New(pool)` in main.go, arms in
// ~/talyvor-queue/w61-replica-invariant-controls-h2r7.py: `auth.New(replicaPool)` on one
// line was CAUGHT, the same call split across lines was MISSED, and so was
// `authPool := replicaPool` followed by `auth.New(authPool)`. auth.New is the revoked-key
// authz store, so the miss means a revoked API key keeps authenticating for the length of
// the replica lag — and main.go already writes 25 constructor calls across several lines,
// so nothing exotic is required. The argument expression is now read from the AST and the
// replica is followed through aliases, so spelling and spacing cannot hide it.
func TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica(t *testing.T) {
	scan, err := scanReplicaWiring("main.go", []byte(mainGoSource(t)))
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// Vacuity floor: a scan that found no calls at all would report no violations.
	// 1,3xx calls measured in main.go at #517; the floor is deliberately loose.
	if len(scan.calls) < 200 {
		t.Fatalf("scanReplicaWiring found only %d calls in main.go — the scanner is blind, not the file empty", len(scan.calls))
	}
	if !scan.tainted[replicaPoolIdent] {
		t.Fatalf("%q is not in the taint set — the scanner would follow nothing", replicaPoolIdent)
	}
	moneyAuthzTx := []string{
		"auth.New",                  // T1 — revoked-key authz
		"budgets.NewStore",          // writer + spend
		"budget.New",                // T4 — per-request token cap
		"billing.New",               // T2 — Stripe credit / wsExists / ensureCustomer
		"workspace.New",             // config feeds cache-pooling privacy (authz-adjacent)
		"attribution.NewStore",      // writer (INSERT request_attribution)
		"routedecision.NewWriter",   // KEEL econ — descriptive WRITER (INSERT routing_decisions); must stay on primary, never the replica
		"economy.NewDualTokenStore", // T3 GetLXCBalance + mint/convert
		"economy.NewRateEngine",
		"economy.NewMarketplaceStore",
		"mining.NewLedgerStore",
		"mining.NewComputeMiner",
		"mining.NewEmbeddingMiner",
		"mining.NewAnnotationMiner",
		"mining.NewPatternMiner",
		"poolroyalty.NewMinter",
		"poolroyalty.NewRevoker",
		"poolroyalty.NewAdjudicationWriter",
		"poolroyalty.NewFinalizeSweeper",
		"oracle.New",
		"provenance.NewBondManager", // H5.β — provenance bonds; slash BURNS collateral (the money path)
		"attest.NewAttestor",        // H5.β step 3 — writes the attested verdict that authorizes a burn
	}
	banned := make(map[string]bool, len(moneyAuthzTx))
	for _, c := range moneyAuthzTx {
		banned[c] = true
	}
	for _, c := range scan.calls {
		if banned[c.name] && c.replicaArg != "" {
			t.Errorf("INVARIANT VIOLATION: money/authz/tx constructor %s receives the replica pool at %s\n  argument: %s",
				c.name, c.pos, c.replicaArg)
		}
	}
}

// execer / txBeginner are the write surfaces a replica must never be handed.
type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}
type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// TestReplicaReaders_AreWriteFree — the five structs that HOLD the replica pool
// expose no write path (no Exec, no Begin). forecast.Store is write-free only
// because of its read-only budgetSpend seam (pinned separately in
// forecast/seam_guard_test.go); this guard pins the public method set of all
// five. If one later gains a write method, giving it the replica could let a
// write reach a lagging replica — caught here at test time.
func TestReplicaReaders_AreWriteFree(t *testing.T) {
	readers := map[string]any{
		"forecast.Store":       forecast.NewStore(nil),
		"costanomaly.Store":    costanomaly.NewStore(nil),
		"anomaly.Detector":     anomaly.New(nil),
		"distillattrib.Reader": distillattrib.NewReader(nil),
		// U25 Keel: holds the replica pool at both keel readers. Query-only readDB
		// seam (no Exec/Begin reachable) — pinned write-free here like the other four.
		"keel.Reader": keel.NewReader(nil),
	}
	for name, r := range readers {
		if _, ok := r.(execer); ok {
			t.Errorf("%s exposes Exec — must be write-free to hold the replica pool", name)
		}
		if _, ok := r.(txBeginner); ok {
			t.Errorf("%s exposes Begin — must be write-free to hold the replica pool", name)
		}
	}
}
