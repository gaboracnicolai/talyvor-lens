package poolroyalty

import (
	"fmt"
	"testing"
)

// THE CONSENSUS FLOOR IS NOW ENFORCED BY A TEST, NOT MERELY DECLARED.
//
// W4.33 (32fc720) pinned DefaultMinConsensusAttesters so its VALUE cannot drift
// silently. That is not the same as proving anything depends on it.
//
// MEASURED 2026-08-28 (tab-k2w8, W4.36) with a reach probe that mutates the
// constant and runs all 114 packages with internal/defaultsguard EXCLUDED —
// excluded because W4.33's own guard reds on any change to these constants and
// would otherwise make every floor read as enforced
// (~/talyvor-queue/w436-reach-probe-k2w8.py).
//
// RESULT: DefaultMinConsensusAttesters 2 -> 1 was noticed by NOTHING. The probe
// itself was positive-controlled 3/3 (both mint rates and the semantic
// threshold each red a real package), and its SIBLING floor in THIS package —
// DefaultMinUnlinkedGraders 3 -> 1 — DID red poolroyalty. So this is not "the
// package is untested"; it is this constant specifically.
//
// ⚠ WHY THE GATE'S OWN TESTS DID NOT COVER IT, and it is an easy shape to
// repeat: eval_consensus_gate_test.go is thorough — self-consensus, sockpuppet
// collapse, self-certified-wrong all pinned — and every one of them passes the
// LITERAL 2 as minAttesters. The gate is well tested; the CONSTANT is what
// nothing bound. A test that hardcodes the threshold it is testing cannot
// notice that threshold changing.
//
// ⚠ WHAT WAS AND WAS NOT WRONG: the constant IS wired to production —
// NewEvalContributionMinter sets minConsensus: DefaultMinConsensusAttesters,
// and evalConsensusReached falls back to it when handed <= 0. Nothing here was
// inert. What was missing was any test whose OUTCOME depended on its value, so
// lowering it to 1 — a single attester minting — was a silent one-token change.

// TestEvalConsensus_ASingleAttesterCannotMint is the load-bearing assertion. It
// passes the SHIPPED constant rather than a literal, so dropping the floor to 1
// flips this from refuse to accept.
func TestEvalConsensus_ASingleAttesterCannotMint(t *testing.T) {
	g := NewIdentityGraph() // A and B are unlinked ⇒ independent operators
	ok, _ := evalConsensusReached([]evalAttestation{att("B", true)}, g, "A", DefaultMinConsensusAttesters)
	if ok {
		t.Fatalf("ONE independent attester cleared the consensus gate at the shipped floor "+
			"(DefaultMinConsensusAttesters = %d). A single agreeing operator must never be able "+
			"to mint an eval item — that is the whole point of the floor.", DefaultMinConsensusAttesters)
	}
}

// TestEvalConsensus_TheFloorIsSatisfiableAtItsOwnValue stops the assertion above
// from being satisfiable by breaking the gate outright. Exactly
// DefaultMinConsensusAttesters independent agreeing operators MUST reach
// consensus — so "refuses one" cannot be achieved by refusing everything.
func TestEvalConsensus_TheFloorIsSatisfiableAtItsOwnValue(t *testing.T) {
	g := NewIdentityGraph()
	attns := make([]evalAttestation, 0, DefaultMinConsensusAttesters)
	for i := 0; i < DefaultMinConsensusAttesters; i++ {
		attns = append(attns, att(fmt.Sprintf("op-%d", i), true))
	}
	ok, reason := evalConsensusReached(attns, g, "A", DefaultMinConsensusAttesters)
	if !ok {
		t.Fatalf("exactly %d independent agreeing operators must reach consensus, got refused: %s",
			DefaultMinConsensusAttesters, reason)
	}
}

// TestEvalContributionMinter_DefaultsWireTheFloors covers the other half of
// "enforced": that the constants reach the object production actually runs.
// evalConsensusReached only falls back to the constant when handed <= 0, so the
// live value is whatever the minter was built with.
func TestEvalContributionMinter_DefaultsWireTheFloors(t *testing.T) {
	m := NewEvalContributionMinter(nil, nil, 0, func() bool { return false })
	if m.minConsensus != DefaultMinConsensusAttesters {
		t.Errorf("minter built with minConsensus = %d, want DefaultMinConsensusAttesters = %d — "+
			"the floor pinned in internal/defaultsguard would not be the floor production applies",
			m.minConsensus, DefaultMinConsensusAttesters)
	}
	if m.minGraders != DefaultMinUnlinkedGraders {
		t.Errorf("minter built with minGraders = %d, want DefaultMinUnlinkedGraders = %d",
			m.minGraders, DefaultMinUnlinkedGraders)
	}
	if !m.requireConsensus {
		t.Error("requireConsensus defaults to false — the consensus gate would be off by default, " +
			"and both assertions above would be pinning a floor nothing consults")
	}
}
