package catalog

import (
	"testing"
)

// A REPRICE MUST NOT WITHDRAW A CAPABILITY.
//
// cmd/lens/main.go advertises LENS_MODEL_CATALOG_OVERRIDES as the way an operator can "add or reprice
// models without a rebuild". A reprice states a price. Decoded into a fresh Model and handed to put —
// which replaces the whole entry — every fact the catalog held about that model decoded as its zero
// value and won: provider "", capabilities all-false, context 0, aliases nil, display name "".
//
// The product consequence is pinned where it lands, in internal/proxy (a 200 streaming vision request
// to gpt-4o became a 422 after a price-only override). This file pins the MECHANISM: what an override
// document is allowed to change, and what it must leave alone.
//
// ⚠ The seed here is a struct literal on a LOCAL registry rather than the shipped catalog, so the
// assertions cannot pass by accident of a seed edit, and nothing here mutates the global registry the
// money path reads.
func newRepriceReg() *Registry {
	return NewRegistry([]Model{{
		ID: "gpt-4o", Provider: "openai", DisplayName: "GPT-4o",
		InputPer1M: 2.50, OutputPer1M: 10.00, CachedInputPer1M: 1.25, CacheWritePer1M: 2.50,
		Capabilities:  Capabilities{Vision: true},
		ContextTokens: 128000, MaxOutput: 16384,
		Aliases: []string{"gpt-4o-2024-11-20"},
	}})
}

func TestDecodeOverrides_RepriceKeepsEveryFactItDidNotState(t *testing.T) {
	r := newRepriceReg()

	// CONTROL: the seeded model really does carry the facts this test is about. Without this a
	// registry that lost them at construction would score identically to one that kept them.
	if m, ok := r.Get("gpt-4o"); !ok || !m.Capabilities.Vision || m.Provider != "openai" || m.ContextTokens != 128000 {
		t.Fatalf("CONTROL: seeded gpt-4o = %+v ok=%v — this test is measuring nothing", m, ok)
	}

	// The exact operator action main.go documents: a price, and nothing else.
	overrides, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	m, ok := r.Get("gpt-4o")
	if !ok {
		t.Fatal("gpt-4o vanished from the catalog after a reprice")
	}
	if !m.Capabilities.Vision {
		t.Error("vision was WITHDRAWN by a price change — modality.Supports now refuses image requests to this model")
	}
	if m.Provider != "openai" {
		t.Errorf("provider = %q, want openai — an empty provider drops the model out of its own fallback anchor set (fallbackRates filters on m.Provider)", m.Provider)
	}
	if m.ContextTokens != 128000 || m.MaxOutput != 16384 {
		t.Errorf("context/max-output = %d/%d, want 128000/16384", m.ContextTokens, m.MaxOutput)
	}
	if m.DisplayName != "GPT-4o" {
		t.Errorf("display name = %q, want GPT-4o", m.DisplayName)
	}
	if len(m.Aliases) != 1 || m.Aliases[0] != "gpt-4o-2024-11-20" {
		t.Errorf("aliases = %v, want [gpt-4o-2024-11-20]", m.Aliases)
	}

	// ByProvider mirrors the FALLBACK-ANCHOR population (fallbackRates selects by the same m.Provider
	// predicate). ⚠ NOT the redirect's — that walks modality.providerPreference, a hardcoded list, and
	// ByProvider has no production caller; the reachable-set gap that follows from that is pinned in
	// internal/proxy/modality_redirect_reach_test.go. A stripped provider removes the model
	// from it silently, which is why this is asserted from the population and not from the field alone.
	byProv := r.ByProvider("openai")
	if len(byProv) != 1 || byProv[0].ID != "gpt-4o" {
		t.Errorf("ByProvider(openai) = %+v, want exactly [gpt-4o]", byProv)
	}

	// And the thing the operator actually asked for must have happened.
	if !approxEq(m.InputPer1M, 3.75) || !approxEq(m.OutputPer1M, 15.00) {
		t.Errorf("prices = %v/%v, want the stated 3.75/15.00", m.InputPer1M, m.OutputPer1M)
	}
}

// MONEY NEUTRALITY, PINNED IN THE DIRECTION IT WOULD DRIFT.
//
// The four price fields are deliberately NOT carried across a reprice. An omitted cache rate must read
// as unset — PriceDetailed falls it back to the NEW input rate ("never free, never under-stated") —
// and must NOT keep the old absolute figure, which was a proportion of the OLD list price. Preserving
// them is the tidier-looking change and it is a pricing decision: at a 2.50 → 3.75 reprice the old
// 1.25 cache-read rate silently drops from 50% of list to 33%.
//
// This case reds if anyone widens the merge to the price fields, which is exactly the "improvement"
// a later reader is most likely to make.
func TestDecodeOverrides_RepriceDoesNotCarryTheOldCacheRates(t *testing.T) {
	r := newRepriceReg()

	if _, cin, cw, _, _ := r.PriceDetailed("gpt-4o"); !approxEq(cin, 1.25) || !approxEq(cw, 2.50) {
		t.Fatalf("CONTROL: seeded cache rates = %v/%v, want 1.25/2.50 — this test assumes explicit seeded rates", cin, cw)
	}

	overrides, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	in, cin, cw, out, ok := r.PriceDetailed("gpt-4o")
	if !ok {
		t.Fatal("gpt-4o vanished")
	}
	if !approxEq(in, 3.75) || !approxEq(out, 15.00) {
		t.Fatalf("stated rates = %v/%v, want 3.75/15.00", in, out)
	}
	if !approxEq(cin, 3.75) || !approxEq(cw, 3.75) {
		t.Errorf("cache rates = %v/%v after a reprice that omitted them, want both 3.75 (the NEW input rate). "+
			"Carrying the pre-reprice 1.25/2.50 forward would re-base the cache discount against a list "+
			"price that no longer exists — a pricing decision, not a decode rule", cin, cw)
	}

	// An override may of course STATE a cache rate, and then it is the operator's number.
	r2 := newRepriceReg()
	ov2, err := r2.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00,"cached_input_per_1m":1.875}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r2.LoadOverrides(ov2)
	if _, cin2, _, _, _ := r2.PriceDetailed("gpt-4o"); !approxEq(cin2, 1.875) {
		t.Errorf("stated cached rate = %v, want 1.875", cin2)
	}
}

// A FACT MUST STILL BE SETTABLE — otherwise "preserve" has quietly become "ignore".
//
// This is the must-stay-green companion to the reprice case: an operator whose provider really did
// withdraw vision states it, and the catalog takes it. Nested-struct decoding into an existing value
// merges field by field, so a capabilities object that names only vision leaves audio/document alone.
func TestDecodeOverrides_AnExplicitFactStillWins(t *testing.T) {
	r := newRepriceReg()
	overrides, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":2.50,"output_per_1m":10.00,` +
		`"capabilities":{"vision":false},"provider":"openai-eu","context_tokens":64000}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	m, _ := r.Get("gpt-4o")
	if m.Capabilities.Vision {
		t.Error("an explicit \"vision\":false was ignored — the merge has become an override the operator cannot use")
	}
	if m.Provider != "openai-eu" {
		t.Errorf("provider = %q, want the stated openai-eu", m.Provider)
	}
	if m.ContextTokens != 64000 {
		t.Errorf("context = %d, want the stated 64000", m.ContextTokens)
	}
	// Unstated facts still survive alongside the stated ones.
	if m.MaxOutput != 16384 || m.DisplayName != "GPT-4o" {
		t.Errorf("unstated facts were dropped: max_output=%d display=%q", m.MaxOutput, m.DisplayName)
	}
}

// AN ID THE CATALOG DOES NOT HOLD MUST DECODE EXACTLY AS BEFORE.
//
// There is no truth to preserve for a brand-new model, and inventing one — inheriting some neighbour's
// capabilities — would be a worse failure than the one this change removes. The zero Model is the
// conservative base and stays the base.
func TestDecodeOverrides_NewModelInheritsNothing(t *testing.T) {
	r := newRepriceReg()
	overrides, err := r.DecodeOverrides([]byte(`[{"id":"acme-1","input_per_1m":1.00,"output_per_1m":2.00}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	m, ok := r.Get("acme-1")
	if !ok {
		t.Fatal("acme-1 was not registered")
	}
	if m.Provider != "" || m.Capabilities.Vision || m.ContextTokens != 0 || len(m.Aliases) != 0 {
		t.Errorf("new model inherited facts from nowhere: %+v", m)
	}
	if !approxEq(m.InputPer1M, 1.00) || !approxEq(m.OutputPer1M, 2.00) {
		t.Errorf("prices = %v/%v, want 1.00/2.00", m.InputPer1M, m.OutputPer1M)
	}
}

// AN OVERRIDE WRITTEN AGAINST AN ALIAS IS NOT MERGED ONTO ITS CANONICAL ENTRY.
//
// Lookup is by exact id. Resolving the alias first would rewrite the alias index as a side effect of a
// reprice — put re-registers every alias of the merged model against the NEW key — which is the same
// class of silent consequence this change exists to remove. Today an alias-keyed override shadows the
// alias and leaves the canonical entry alone; that is pinned here so it cannot drift unnoticed.
func TestDecodeOverrides_AliasKeyedOverrideDoesNotTouchTheCanonicalEntry(t *testing.T) {
	r := newRepriceReg()
	overrides, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o-2024-11-20","input_per_1m":9.00,"output_per_1m":9.00}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	canon, _ := r.Get("gpt-4o")
	if !approxEq(canon.InputPer1M, 2.50) || !canon.Capabilities.Vision {
		t.Errorf("the canonical gpt-4o was modified by an alias-keyed override: %+v", canon)
	}
	shadow, ok := r.Get("gpt-4o-2024-11-20")
	if !ok || !approxEq(shadow.InputPer1M, 9.00) {
		t.Errorf("alias-keyed override = %+v ok=%v, want its own entry at 9.00", shadow, ok)
	}
	if shadow.Capabilities.Vision {
		t.Error("the alias entry inherited the canonical model's capabilities — lookup is supposed to be by exact id")
	}
}

// #449's RULE MUST SURVIVE THE MERGE: a KNOWN model repriced to nothing is still unpriced.
//
// This is the regression that the merge could plausibly reintroduce — if the old rates were carried
// forward, an operator who priced a model at nothing would silently keep billing the old price under
// an `exact` provenance instead of falling to the loud floor. It reds if the price fields ever start
// surviving a decode.
//
// ⚠ THE FIXTURE WAS RE-ANCHORED, THE RULE WAS NOT WEAKENED (override_unknown_field_test.go).
// This test used to reach the zero-rate state through a camelCase TYPO
// (`{"id":"gpt-4o","inputPer1M":3.75,"outputPer1M":15.00}`), which DecodeOverrides now refuses
// outright with DisallowUnknownFields — so the typo never gets far enough to exercise #449's rule,
// and the old fixture would have failed at the decode instead of testing anything.
//
// The rule itself is unchanged and is exercised here through the document that still REACHES it:
// an explicit reprice to zero. That is now the only way to register a known model with no price, so
// it is the right fixture — and it is a stronger one, because it tests the rule on a document the
// operator MEANT rather than on one they fat-fingered. The typo's own behaviour (refused, naming
// the field) is asserted in override_unknown_field_test.go; the two are complementary, and the
// change of trigger is recorded here rather than left for someone to rediscover.
func TestDecodeOverrides_MistypedRepriceOfAKnownModelIsStillUnpriced(t *testing.T) {
	r := newRepriceReg()
	overrides, err := r.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":0,"output_per_1m":0}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	r.LoadOverrides(overrides)

	rates, prov := r.ResolveRates("gpt-4o", PurposeCharge)
	if prov != ProvenanceFallback {
		t.Errorf("provenance = %v, want fallback — a model priced at nothing registers no price and must not report one as published", prov)
	}
	if rates.InputPer1M <= 0 || rates.OutputPer1M <= 0 {
		t.Errorf("ResolveRates returned a spendable zero %+v", rates)
	}
	// The facts are still there — that is the point of the merge — but they do not make it priced.
	if m, _ := r.Get("gpt-4o"); !m.Capabilities.Vision {
		t.Error("the zero reprice also withdrew vision")
	}
}
