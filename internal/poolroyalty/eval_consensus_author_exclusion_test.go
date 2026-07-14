package poolroyalty

import "testing"

// TestEvalConsensus_AuthorComponentExcluded_SockpuppetIsNotAnIndependentVote isolates the AUTHOR-COMPONENT
// exclusion in evalConsensusReached — the one farm defense the other consensus tests do NOT actually cover.
//
// Scenario: author A; attesters = A2 (A's OWN sockpuppet, transitively linked, agrees) + B (a genuinely
// independent operator, agrees); minAttesters = 2.
//   - WITH the author-exclusion (correct): A2 is dropped as SameOperator(A); only B's operator votes → 1 < 2
//     → NOT reached. The author cannot buy the deciding second vote with its own sockpuppet.
//   - WITHOUT it (mutation): A's component (A2 agrees) + B → 2 votes → reached — a self-consensus farm.
//
// This case FLIPS on the `|| g.SameOperator(a.workspace, author)` clause, unlike
// TestEvalConsensus_SameOperatorCannotSelfConsense and the acceptance `sock` case — whose sockpuppets all
// collapse to ONE component and so never reach 2 regardless of the author-exclusion. Those two pass on the
// one-vote-per-component defense, leaving the author-exclusion itself unverified; deleting it is invisible to
// CI without THIS test.
func TestEvalConsensus_AuthorComponentExcluded_SockpuppetIsNotAnIndependentVote(t *testing.T) {
	g := NewIdentityGraph()
	g.Link("A", "A2") // A2 is the author's own sockpuppet (same operator, transitively)
	ok, reason := evalConsensusReached([]evalAttestation{att("A2", true), att("B", true)}, g, "A", 2)
	if ok {
		t.Fatalf("author sockpuppet A2 must NOT count as the 2nd independent vote (self-consensus farm) — reason=%q", reason)
	}
}
