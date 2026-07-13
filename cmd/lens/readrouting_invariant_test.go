package main

import (
	"context"
	"os"
	"strings"
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
func TestReadReplicaWiring_ExactlySixAnalyticsReaders(t *testing.T) {
	src := mainGoSource(t)
	const call = "dbrouting.ReadPool(pool, replicaPool)"
	if n := strings.Count(src, call); n != 6 {
		t.Fatalf("expected EXACTLY 6 analytics readers routed via %s, found %d — the replica wiring set changed; re-review the invariant table", call, n)
	}
	for _, reader := range []string{
		"forecast.NewStore(dbrouting.ReadPool(pool, replicaPool))",
		"costanomaly.NewStore(dbrouting.ReadPool(pool, replicaPool))",
		"anomaly.New(dbrouting.ReadPool(pool, replicaPool))",
		"distillattrib.NewReader(dbrouting.ReadPool(pool, replicaPool))",
		// Keel (U25) drift attribution — this identical call appears TWICE in main.go
		// (the leader-elected drift-sweep corpus reader ~L576, and the requireAdmin
		// findings read handler ~L1300), which is why the count above is 6, not 5.
		// Both are READ-ONLY analytics over the consented corpus (routing_patterns)
		// + keel_findings and are replica-lag-tolerant; the keel_findings WRITE goes
		// to the PRIMARY via FindingsWriter(pool), never through the replica.
		"keel.NewReader(dbrouting.ReadPool(pool, replicaPool))",
	} {
		if !strings.Contains(src, reader) {
			t.Errorf("missing expected analytics→replica wiring: %s", reader)
		}
	}
}

// TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica — THE invariant: no
// money/authz/tx constructor is on a line that references the replica pool. A
// careless future edit (e.g. billing.New(dbrouting.ReadPool(...))) trips this.
func TestReadReplicaWiring_MoneyAuthzNeverReceiveReplica(t *testing.T) {
	src := mainGoSource(t)
	moneyAuthzTx := []string{
		"auth.New",                  // T1 — revoked-key authz
		"budgets.NewStore",          // writer + spend
		"budget.New",                // T4 — per-request token cap
		"billing.New",               // T2 — Stripe credit / wsExists / ensureCustomer
		"workspace.New",             // config feeds cache-pooling privacy (authz-adjacent)
		"attribution.NewStore",      // writer (INSERT request_attribution)
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
	}
	for _, line := range strings.Split(src, "\n") {
		if !strings.Contains(line, "replicaPool") && !strings.Contains(line, "dbrouting.ReadPool") {
			continue
		}
		for _, ctor := range moneyAuthzTx {
			if strings.Contains(line, ctor+"(") {
				t.Errorf("INVARIANT VIOLATION: money/authz/tx constructor %s is on a replica-pool line:\n  %s", ctor, strings.TrimSpace(line))
			}
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
