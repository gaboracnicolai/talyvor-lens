// Package routingbrain is the H8.1 Routing Brain. It learns OFFLINE from verified
// routing outcomes — H1 WorkTier classification (the cohort), H2 model-capability
// curves (quality-vs-tier), and Keel drift attribution (cross-tenant verified
// quality) — and produces, per (workspace, request-shape/cohort), a recommendation
// of the best model. The serving path only READS the latest precomputed
// recommendation (a cheap in-memory lookup, never a computation).
//
// It is DESCRIPTIVE + MINT-FREE: it reads outcomes and advises routing. It NEVER
// mints, debits, or writes a ledger row — the packages here import no money/ledger
// package (pinned by the import guard) and the store holds only Exec/Query seams.
// A recommendation informs routing; it is advice, never currency.
//
// Two postures over the SAME recommendation object:
//   - ADVISORY (default): the recommendation is SURFACED; the existing router still
//     decides. The chosen model is byte-for-byte the router's.
//   - AUTONOMOUS (explicit per-workspace opt-in): the recommendation is APPLIED —
//     the brain's model becomes the route — but ONLY within the HARD FLOOR below.
//
// THE HARD FLOOR autonomous can never cross (enforced in floorOK, proven live by
// mutation): (i) never exceed the workspace cost cap — here the per-request cost of
// the safe existing decision, so autonomous can re-rank but never RAISE the bill;
// (ii) never route to an unverified/unsafe model — one Keel did not verify OR one
// not in the workspace's allow-list. Breach either → fall back to the safe decision.
// Autonomous = "trusted to pick within bounds," never "bounds removed."
package routingbrain

// Mode is a workspace's brain posture.
type Mode int

const (
	ModeAdvisory   Mode = iota // default: surface only, never apply
	ModeAutonomous             // apply the brain's pick, subject to the hard floor
)

// Recommendation is one precomputed (workspace, difficulty) pick from the offline
// brain. Verified records that it passed the Keel-drift + allow-list checks AT
// COMPUTE TIME; the serve-time hard floor re-checks the live allow-list and cost.
type Recommendation struct {
	WorkspaceID     string  `json:"workspace_id"`
	Difficulty      int     `json:"difficulty"`
	Model           string  `json:"model"`
	Provider        string  `json:"provider"`
	ExpectedQuality float64 `json:"expected_quality"`
	Verified        bool    `json:"verified"`
	Reason          string  `json:"reason"`
}

// SafeDecision is the model the existing router would route to WITHOUT the brain,
// plus its per-request cost (same blended basis the floor uses for the brain pick).
type SafeDecision struct {
	Model string
	Cost  float64
}

// Decision is the resolved routing outcome.
type Decision struct {
	Model    string // the model to actually route to
	Applied  bool   // the brain's pick replaced the safe decision (autonomous + floor passed)
	Surfaced bool   // a recommendation exists (advisory-surfaced OR autonomous)
	Reason   string
}

// Decide resolves the final model. PURE / deterministic:
//   - no recommendation      → route the safe decision; surface nothing.
//   - ADVISORY               → ALWAYS route the safe decision; surface as advice.
//   - AUTONOMOUS + floor pass → route the brain's model.
//   - AUTONOMOUS + floor fail → fall back to the safe decision (surfaced).
//
// recCost is the brain model's per-request cost on the SAME basis as safe.Cost;
// allowedModels is the workspace's CURRENT allow-list (re-checked live).
func Decide(mode Mode, rec *Recommendation, safe SafeDecision, recCost float64, allowedModels []string) Decision {
	if rec == nil || rec.Model == "" {
		return Decision{Model: safe.Model}
	}
	if mode == ModeAdvisory {
		return Decision{Model: safe.Model, Surfaced: true, Reason: "advisory: " + rec.Reason}
	}
	if floorOK(rec, safe, recCost, allowedModels) {
		return Decision{Model: rec.Model, Applied: true, Surfaced: true, Reason: "autonomous applied: " + rec.Reason}
	}
	return Decision{Model: safe.Model, Surfaced: true, Reason: "autonomous fallback (hard floor): " + floorViolation(rec, safe, recCost, allowedModels)}
}

// floorOK is THE HARD FLOOR — the single gate autonomous can never cross. All three
// conditions must hold. This function is the mutation target: neuter it and the
// floor tests must fail.
func floorOK(rec *Recommendation, safe SafeDecision, recCost float64, allowedModels []string) bool {
	if !rec.Verified {
		return false // (ii) unverified — Keel did not clear this model for this workspace
	}
	if !contains(allowedModels, rec.Model) {
		return false // (ii) unsafe — not in the workspace's allow-list
	}
	if recCost > safe.Cost {
		return false // (i) exceeds the cost cap — pricier than the safe decision
	}
	return true
}

// floorViolation names WHY the floor rejected the pick (for the fallback reason).
func floorViolation(rec *Recommendation, safe SafeDecision, recCost float64, allowedModels []string) string {
	switch {
	case !rec.Verified:
		return "unverified model"
	case !contains(allowedModels, rec.Model):
		return "model not in workspace allow-list"
	case recCost > safe.Cost:
		return "exceeds cost cap (safe decision)"
	default:
		return "ok" // unreachable when called on a rejected pick
	}
}

func contains(hs []string, needle string) bool {
	for _, h := range hs {
		if h == needle {
			return true
		}
	}
	return false
}
