package poolroyalty

import "testing"

// The consensus core, isolated from PG: independence is bound by the transitive identity graph. These pin
// the three farm defenses the task requires — same-operator cannot self-consense, a self-certified WRONG
// answer (independent disagreement) cannot pass, and sockpuppets in one operator collapse to one vote.

func att(ws string, agrees bool) evalAttestation {
	return evalAttestation{workspace: ws, agrees: agrees}
}

// HAPPY: two INDEPENDENT operators agree the claimed answer is correct → consensus.
func TestEvalConsensus_TwoIndependentAgree_Reached(t *testing.T) {
	g := NewIdentityGraph() // A, B, C all singletons ⇒ three distinct operators
	ok, reason := evalConsensusReached([]evalAttestation{att("B", true), att("C", true)}, g, "A", 2)
	if !ok {
		t.Fatalf("two independent operators agree → consensus expected, got: %s", reason)
	}
}

// FARM PROOF 1 — SAME-OPERATOR CANNOT SELF-CONSENSE: the author funds sockpuppets A2, A3 (linked to the
// author A, transitively). They are all SameOperator(A) → excluded → zero independent agreement → no mint.
func TestEvalConsensus_SameOperatorCannotSelfConsense(t *testing.T) {
	g := NewIdentityGraph()
	g.Link("A", "A2") // shared card / owner_key
	g.Link("A2", "A3")
	ok, _ := evalConsensusReached([]evalAttestation{att("A2", true), att("A3", true)}, g, "A", 2)
	if ok {
		t.Fatal("author's own sockpuppets (transitively linked) must NOT reach consensus — self-consensus is impossible")
	}
}

// FARM PROOF 3 — SELF-CERTIFIED WRONG ANSWER CANNOT MINT: independent operators B, C examine the claimed
// answer and DISAGREE (the answer is wrong). Zero agreeing → no consensus → the item never earns.
func TestEvalConsensus_SelfCertifiedWrong_Rejected(t *testing.T) {
	g := NewIdentityGraph()
	ok, _ := evalConsensusReached([]evalAttestation{att("B", false), att("C", false)}, g, "A", 2)
	if ok {
		t.Fatal("independent operators disagree with the claimed answer → consensus must NOT be reached (wrong answer cannot mint)")
	}
}

// SOCKPUPPET COLLAPSE: many workspaces in ONE operator cast ONE vote — they cannot manufacture the
// independent-count. B and B2 are one operator; only with a genuinely separate operator C is 2 reached.
func TestEvalConsensus_LinkedAttestersCollapseToOneVote(t *testing.T) {
	g := NewIdentityGraph()
	g.Link("B", "B2") // B and B2 are the SAME operator
	if ok, _ := evalConsensusReached([]evalAttestation{att("B", true), att("B2", true)}, g, "A", 2); ok {
		t.Fatal("two workspaces of one operator must count as ONE independent vote, not two")
	}
	// Add a genuinely independent operator C → now two distinct operators agree → consensus.
	if ok, reason := evalConsensusReached([]evalAttestation{att("B", true), att("B2", true), att("C", true)}, g, "A", 2); !ok {
		t.Fatalf("one operator (B,B2) + one independent operator (C) = 2 independent agreements → consensus expected, got: %s", reason)
	}
}

// CONTESTED: a split among independent operators (1 agree, 2 disagree) fails the agreement floor even if
// the raw agree-count were lowered — a contested answer is not trustworthy.
func TestEvalConsensus_ContestedBelowFloor_Rejected(t *testing.T) {
	g := NewIdentityGraph()
	ok, _ := evalConsensusReached([]evalAttestation{att("B", true), att("C", false), att("D", false)}, g, "A", 1)
	if ok {
		t.Fatal("1 agree vs 2 disagree among independent operators is below the agreement floor → not consensus")
	}
}
