package catalog

import "strings"

// UNPRICED MUST BE UNREPRESENTABLE ON THE MONEY PATH.
//
// Price and PriceDetailed return ok=false for an unknown model, and their doc comments say callers
// "price an unknown model at 0". Eleven callers handled that correctly because a zero is harmless in
// telemetry. The billing path did not, and the result was that a model the provider serves and this
// catalog does not know was served FREE: no hold, no spend row, no ledger trace, and Talyvor holding
// the provider bill.
//
// The lesson is not "check ok more carefully". It is that a signature which can hand back a spendable
// zero will eventually be spent. So the money path does not use those functions at all — it uses
// ResolveRates, which HAS NO ZERO OUTCOME. There is nothing to mishandle: every return is chargeable,
// and the Provenance says whether it was measured or guessed.

// Rates is a model's per-1M-token prices. It is only ever produced by ResolveRates, and never with
// zero values — see the package invariant above.
type Rates struct {
	InputPer1M       float64
	CachedInputPer1M float64
	CacheWritePer1M  float64
	OutputPer1M      float64
}

// Provenance records HOW a Rates was arrived at. There is deliberately no zero value in the iota:
// a Provenance of 0 is not a valid state, so a struct that forgot to set it cannot masquerade as
// exact pricing.
type Provenance uint8

const (
	// ProvenanceExact — the catalog knows this model. The rates are its published prices.
	ProvenanceExact Provenance = iota + 1
	// ProvenanceFallback — the catalog does NOT know this model and the rates are a derived bound.
	// A charge on this basis is a guess and must be marked as one wherever it lands.
	ProvenanceFallback
)

func (p Provenance) String() string {
	switch p {
	case ProvenanceExact:
		return "exact"
	case ProvenanceFallback:
		return "fallback"
	}
	return "invalid"
}

// Purpose selects WHICH bound an unknown model falls back to, because "conservative" points in
// opposite directions for a hold and for a charge:
//
//   - A HOLD is refundable. Holding too much costs the customer nothing permanently (the settle
//     refunds the difference) while holding too little leaks the sub-budget ceiling. So a hold falls
//     back HIGH — the provider's most expensive known model.
//   - A CHARGE is final. Charging too much is an over-bill on a rate we admittedly guessed, which is
//     the one outcome that is indefensible to a customer. So a charge falls back LOW — the provider's
//     cheapest known model, a floor we can defend line by line.
//
// This asymmetry is the same one the reservation seam already runs on: hold an output-aware upper
// bound, charge what was delivered, clamp the charge to the hold.
type Purpose uint8

const (
	// PurposeHold — a pre-serve reservation. Falls back to the provider's most expensive model.
	PurposeHold Purpose = iota + 1
	// PurposeCharge — the final bill. Falls back to the provider's cheapest model.
	PurposeCharge
)

// ResolveRates returns chargeable rates for any model id, ALWAYS non-zero, plus how they were derived.
//
// ⚠ It has no failure mode by construction. That is the point: a caller cannot accidentally spend an
// "unknown" as free, because there is no unknown to receive. When the catalog does not know the model,
// the rates are a bound derived from the same provider's known models (see Purpose), and Provenance is
// ProvenanceFallback so the caller can mark, alert on, and later reprice the charge.
func (r *Registry) ResolveRates(id string, purpose Purpose) (Rates, Provenance) {
	if in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); ok {
		return Rates{InputPer1M: in, CachedInputPer1M: cachedIn, CacheWritePer1M: cacheWrite, OutputPer1M: out},
			ProvenanceExact
	}
	return r.fallbackRates(id, purpose), ProvenanceFallback
}

// fallbackRates derives a bound from the models this catalog DOES know for the same provider, so the
// number is anchored in real published prices rather than invented. Provider is inferred from the id
// prefix; when that fails (a shape we have never seen) the bound is taken across the WHOLE catalog,
// which is still a real price and still non-zero.
func (r *Registry) fallbackRates(id string, purpose Purpose) Rates {
	r.mu.RLock()
	defer r.mu.RUnlock()

	provider := providerFromID(id)
	best := Rates{}
	found := false
	consider := func(m Model) {
		cand := Rates{InputPer1M: m.InputPer1M, CachedInputPer1M: m.CachedInputPer1M,
			CacheWritePer1M: m.CacheWritePer1M, OutputPer1M: m.OutputPer1M}
		if cand.InputPer1M <= 0 && cand.OutputPer1M <= 0 {
			return // a zero-priced catalog entry cannot anchor a fallback
		}
		if !found {
			best, found = cand, true
			return
		}
		// Rank on output rate then input: output dominates the bill on generative traffic.
		higher := cand.OutputPer1M > best.OutputPer1M ||
			(cand.OutputPer1M == best.OutputPer1M && cand.InputPer1M > best.InputPer1M)
		if (purpose == PurposeHold) == higher {
			best = cand
		}
	}
	for _, m := range r.byID {
		if provider != "" && m.Provider != provider {
			continue
		}
		consider(m)
	}
	if !found && provider != "" {
		// Known-looking prefix but no priced sibling — widen to the whole catalog rather than return zero.
		for _, m := range r.byID {
			consider(m)
		}
	}
	if !found {
		// An empty or wholly unpriced catalog. Refuse to hand back a spendable zero: this is a
		// deployment-configuration failure, and a non-zero floor keeps the money invariant intact
		// while the loud alert at the call site surfaces the real problem.
		return Rates{InputPer1M: emptyCatalogFloorPer1M, CachedInputPer1M: emptyCatalogFloorPer1M,
			CacheWritePer1M: emptyCatalogFloorPer1M, OutputPer1M: emptyCatalogFloorPer1M}
	}
	return best
}

// emptyCatalogFloorPer1M is the last-resort rate when the catalog itself is empty or unpriced. It is
// deliberately small (never a punitive bill) but strictly positive, because zero is the failure this
// whole file exists to remove.
const emptyCatalogFloorPer1M = 0.01

// providerFromID infers the provider from an id prefix. Best-effort by design — a wrong guess only
// changes WHICH real price anchors the bound, never whether one exists.
func providerFromID(id string) string {
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "anthropic"
	case strings.HasPrefix(id, "gpt-"), strings.HasPrefix(id, "o1-"), strings.HasPrefix(id, "o3-"):
		return "openai"
	case strings.HasPrefix(id, "gemini-"):
		return "google"
	case strings.HasPrefix(id, "mistral-"), strings.HasPrefix(id, "magistral-"):
		return "mistral"
	}
	return ""
}

// ResolveRates on the default registry — the entry point the money path uses.
func ResolveRates(id string, purpose Purpose) (Rates, Provenance) {
	return defaultRegistry.ResolveRates(id, purpose)
}
