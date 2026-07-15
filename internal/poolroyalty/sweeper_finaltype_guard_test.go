package poolroyalty

// sweeper_finaltype_guard_test.go — the wiring guard for the settled ledger label.
//
// finalTypeForTable is the ONLY place a claim table becomes a ledger type. A new held mint
// wired to this sweeper without a map entry would be fail-closed at settle (settleOne
// refuses) — safe, but it strands mints until someone notices. This guard moves that
// discovery to CI: it reads the real cmd/lens/main.go and pins every table actually wired
// to NewFinalizeSweeper against the map.
//
// Source-parsing mirrors cmd/lens/readrouting_invariant_test.go, which pins money/authz
// constructors off the replica pool the same way.

import (
	"os"
	"regexp"
	"testing"

	"github.com/talyvor/lens/internal/mining"
)

// newFinalizeSweeperTable captures the table literal from a wired sweeper, e.g.
// poolroyalty.NewFinalizeSweeper(pool, tokenLedger, "node_latency_mints").
var newFinalizeSweeperTable = regexp.MustCompile(`NewFinalizeSweeper\([^)]*"([a-z_]+)"\s*\)`)

func TestFinalTypeForTable_CoversEveryWiredSweeper(t *testing.T) {
	src, err := os.ReadFile("../../cmd/lens/main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	matches := newFinalizeSweeperTable.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no NewFinalizeSweeper(...) wiring in cmd/lens/main.go — the guard's regexp has gone stale, not the wiring")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		table := m[1]
		seen[table] = true
		final, ok := finalTypeForTable[table]
		if !ok {
			t.Errorf("main.go wires a FinalizeSweeper over %q but finalTypeForTable has no entry:\n"+
				"  its settled rows would have no decided ledger type, so they cannot finalize (fail-closed).\n"+
				"  Add %q → its own COUNTED type, add that type to mining.GetTotalSupply's allow-list,\n"+
				"  and keep it OUT of mintTypeList (finalize is a settlement, not a mint moment).", table, table)
			continue
		}
		if final == "" {
			t.Errorf("finalTypeForTable[%q] is empty — a settled row cannot carry an empty ledger type", table)
		}
	}
	// The map must not carry entries for tables nobody wires: a stale key is a settled label
	// that no longer corresponds to a mint, and it would silently bless a typo'd table name.
	for table := range finalTypeForTable {
		if !seen[table] {
			t.Errorf("finalTypeForTable has an entry for %q but cmd/lens/main.go wires no sweeper over it — "+
				"remove the stale key or wire the mint", table)
		}
	}
}

// TestFinalTypeForTable_EveryFinalTypeIsCounted pins the half of the finalize contract that
// keeps this an ATTRIBUTION fix: settlement is when a mint enters supply, so every type the
// sweeper can write must be counted by GetTotalSupply. A final type outside that allow-list
// would silently drop settled mints out of total supply — a label change that moved money.
//
// mining.CountedSupplyTypes is the allow-list GetTotalSupply itself reads, so this cannot
// drift from the real query.
func TestFinalTypeForTable_EveryFinalTypeIsCounted(t *testing.T) {
	counted := map[string]bool{}
	for _, ty := range mining.CountedSupplyTypes() {
		counted[ty] = true
	}
	for table, final := range finalTypeForTable {
		if !counted[final] {
			t.Errorf("finalTypeForTable[%q] = %q, which is NOT counted in GetTotalSupply:\n"+
				"  settling a mint under an uncounted type drops its µLENS out of total supply\n"+
				"  (and out of the LXC conversion math that reads it) — that is a money change, not a relabel.",
				table, final)
		}
	}
}

// TestFinalTypeForTable_NoFinalTypeIsAMintMoment pins the other half: finalize settles
// already-gated held value. A final type that is ALSO a mint-moment type would re-run the
// verified-to-earn gate and the rate cap at settlement — double-gating a mint that was
// already gated when it landed held, so a workspace verified at mint time but later
// un-verified could never settle value it legitimately earned.
func TestFinalTypeForTable_NoFinalTypeIsAMintMoment(t *testing.T) {
	for table, final := range finalTypeForTable {
		if mining.IsMintType(final) {
			t.Errorf("finalTypeForTable[%q] = %q is in mintTypeList — finalize would be re-gated + rate-capped;\n"+
				"  the MINT moment is the _held type, the settlement must not be gated again.", table, final)
		}
	}
}
