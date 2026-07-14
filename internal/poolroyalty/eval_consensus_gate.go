// eval_consensus_gate.go — Proof-of-eval-contribution CORRECTNESS CONSENSUS.
//
// The eval-contribution mint pays a contributor for a DISCRIMINATING eval item. Discrimination is a hard,
// externally-observed readout — but it does NOT establish that the contributor's CLAIMED expected_output is
// CORRECT. A self-certified wrong answer that happens to split models could otherwise earn. This gate closes
// that: an item earns ONLY after independent workspaces AGREE its claimed answer is correct — the submitter's
// own assertion is never trusted.
//
// INDEPENDENCE is bound by the SAME transitive identity graph the ring detector uses (card fingerprint ∪
// owner_key, closed under union-find). Attesters collapse to operator COMPONENTS; the author's component is
// excluded, so an operator can NEVER self-consense through its own sockpuppets (a pairwise check would miss a
// A↔A2↔A3 chain — the transitive closure does not). Each independent operator casts ONE vote (majority of its
// own attestations), so N sockpuppets in one operator cannot manufacture the independent count.
//
// This mirrors the annotation-mining consensus, with two deliberate differences the task requires: (1) the
// WHOLE payment is gated on consensus (annotation pays an unconditional base + a gated bonus; here there is no
// unconditional floor — an unconsensed eval earns nothing), and (2) independence is the transitive-closure
// identity graph, not annotation's "other annotators exist" count.
package poolroyalty

import "fmt"

// DefaultMinConsensusAttesters is the correctness floor: an eval item earns nothing until at least this many
// INDEPENDENT operators (distinct identity-graph components, none the author's) attest its claimed answer is
// correct. Mirrors the DefaultMinUnlinkedGraders warmup: a single sockpuppet cannot clear it.
const DefaultMinConsensusAttesters = 2

// evalConsensusAgreementFloor: of the independent operators that voted (agree or disagree), at least this
// fraction must AGREE. A contested answer (independent disagreement) fails even if a few agree — a claimed
// answer other operators actively dispute is not trustworthy enough to mint on.
const evalConsensusAgreementFloor = 2.0 / 3.0

// evalAttestationsSQL reads every correctness attestation for one item (agree or disagree). $1 = item id.
const evalAttestationsSQL = `SELECT attester_workspace_id, agrees FROM eval_correctness_attestations WHERE item_id = $1`

// evalAttestation is one workspace's judgment that an item's claimed answer is (in)correct.
type evalAttestation struct {
	workspace string
	agrees    bool
}

// evalConsensusReached decides whether an item's CLAIMED answer has independent correctness consensus.
//
// Independence is bound by the transitive identity graph g: every attester in the author's component (the
// author itself and its transitively-linked sockpuppets) is EXCLUDED; each remaining operator component casts
// exactly ONE vote = the majority of its own attestations (a tie counts as no-agree, the conservative
// direction). Consensus ⟺ (independent operators AGREEING ≥ minAttesters) AND (agreeing / voting ≥ floor).
// Returns (false, reason) when not reached so the minter can log WHY an item is withheld.
func evalConsensusReached(attns []evalAttestation, g *IdentityGraph, author string, minAttesters int) (bool, string) {
	if g == nil {
		g = NewIdentityGraph()
	}
	if minAttesters <= 0 {
		minAttesters = DefaultMinConsensusAttesters
	}
	// Tally one (agree,disagree) per operator component, excluding the author's own component entirely.
	type tally struct{ agree, disagree int }
	byComponent := map[string]*tally{}
	for _, a := range attns {
		if a.workspace == "" || g.SameOperator(a.workspace, author) {
			continue // author + same-operator sockpuppets cannot vote on their own item
		}
		comp := g.Component(a.workspace)
		t := byComponent[comp]
		if t == nil {
			t = &tally{}
			byComponent[comp] = t
		}
		if a.agrees {
			t.agree++
		} else {
			t.disagree++
		}
	}
	agreeing, voting := 0, 0
	for _, t := range byComponent {
		voting++
		if t.agree > t.disagree {
			agreeing++ // this independent operator agrees the answer is correct
		}
	}
	if agreeing < minAttesters {
		return false, fmt.Sprintf("consensus not reached: %d independent operators agree, need %d (unused/unconsensed items earn zero)", agreeing, minAttesters)
	}
	if float64(agreeing)/float64(voting) < evalConsensusAgreementFloor {
		return false, fmt.Sprintf("consensus not reached: agreement ratio %d/%d below floor %.2f (contested answer)", agreeing, voting, evalConsensusAgreementFloor)
	}
	return true, ""
}
