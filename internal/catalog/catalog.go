// Package catalog is the single source of truth for the models Lens knows
// about — their provider, pricing (USD per 1M tokens), capabilities
// (vision/audio/document), and context limits. Before this, those facts were
// scattered across the alerts price table, the modality capability registry,
// and the router; consolidating them here means adding a model is data, not
// code edits in several places.
//
// Boundary: the catalog owns model FACTS (price, capabilities, context). It
// does NOT own routing POLICY — the router's cost-tier ranks, the cheap/mid/
// premium tiers, fallback chains, and provider dispatch remain in their
// packages and operate on catalog models. (modelRanks is a tier order, not a
// price order, so it isn't derivable from catalog pricing.)
//
// The registry is read-mostly and concurrency-safe: pricing/capability
// lookups happen on the request hot path, so reads take an RWMutex read lock
// and never allocate beyond the returned value. Runtime overrides (config/DB)
// layer on top of the embedded default so an operator can add or reprice a
// model without a rebuild.
package catalog

import (
	"encoding/json"
	"sort"
	"sync"
)

// Capabilities are the non-text modalities a model can serve. Mirrors
// modality.Capabilities (which now reads from here).
type Capabilities struct {
	Vision   bool `json:"vision"`
	Audio    bool `json:"audio"`
	Document bool `json:"document"`
}

// Model is one catalog entry. InputPer1M/OutputPer1M are USD per 1,000,000
// tokens — the canonical pricing the cost-attribution moat depends on.
// ContextTokens/MaxOutput are best-effort informational values (no behavior
// gates on them today); pricing + capabilities are authoritative.
//
// CachedInputPer1M / CacheWritePer1M are the prompt-caching rates (USD per 1M):
// what a provider charges for input tokens served from its cache (a read) and
// for tokens written into its cache (a write). They are seeded from published
// per-provider multipliers on the base input rate (see seed.go withCacheRates)
// and are what lets the cost basis price a cache-heavy request at what Talyvor
// actually paid rather than at the full list rate. A zero value means "unset"
// and PriceDetailed falls it back to the input rate (never free).
type Model struct {
	ID               string       `json:"id"`
	Provider         string       `json:"provider"`
	DisplayName      string       `json:"display_name"`
	InputPer1M       float64      `json:"input_per_1m"`
	OutputPer1M      float64      `json:"output_per_1m"`
	CachedInputPer1M float64      `json:"cached_input_per_1m,omitempty"` // cache READ rate (Anthropic 0.1x input, OpenAI ~0.5x)
	CacheWritePer1M  float64      `json:"cache_write_per_1m,omitempty"`  // cache WRITE rate (Anthropic 1.25x input)
	Capabilities     Capabilities `json:"capabilities"`
	ContextTokens    int          `json:"context_tokens"`
	MaxOutput        int          `json:"max_output"`
	Deprecated       bool         `json:"deprecated,omitempty"`
	Aliases          []string     `json:"aliases,omitempty"` // e.g. dated snapshots → this canonical id
}

// Registry holds the models keyed by canonical id, with an alias index.
type Registry struct {
	mu      sync.RWMutex
	byID    map[string]Model
	aliasTo map[string]string
}

// NewRegistry builds a registry from a seed list.
func NewRegistry(models []Model) *Registry {
	r := &Registry{byID: make(map[string]Model, len(models)), aliasTo: map[string]string{}}
	for _, m := range models {
		r.put(m)
	}
	return r
}

// put inserts/updates a model + its aliases. Caller must hold the write lock
// (or be single-threaded construction).
func (r *Registry) put(m Model) {
	r.byID[m.ID] = m
	for _, a := range m.Aliases {
		r.aliasTo[a] = m.ID
	}
}

// resolve maps an id-or-alias to a canonical id. Caller holds at least RLock.
func (r *Registry) resolve(id string) string {
	if _, ok := r.byID[id]; ok {
		return id
	}
	if canon, ok := r.aliasTo[id]; ok {
		return canon
	}
	return id
}

// Get returns the model for an id or alias.
func (r *Registry) Get(id string) (Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[r.resolve(id)]
	return m, ok
}

// Price returns (input, output) USD per 1M tokens; ok=false for unknown
// models (callers price an unknown model at 0, exactly as before).
func (r *Registry) Price(id string) (in, out float64, ok bool) {
	m, ok := r.Get(id)
	return m.InputPer1M, m.OutputPer1M, ok
}

// PriceDetailed returns the full cache-aware per-1M price breakdown:
//
//	in         — uncached input rate
//	cachedIn   — cache-READ rate (input tokens served from the provider's cache)
//	cacheWrite — cache-WRITE rate (input tokens written into the cache)
//	out        — output rate
//
// in/out are byte-identical to Price for every model (a golden test enforces
// this), so the existing cost basis is never perturbed. ok=false for unknown
// models (priced at 0, exactly as Price). When a model carries no explicit
// cached/write rate — e.g. a runtime Override that set only input/output — those
// fall back to the input rate: the conservative choice that never bills a cached
// token at zero and never under-states cost.
func (r *Registry) PriceDetailed(id string) (in, cachedIn, cacheWrite, out float64, ok bool) {
	m, ok := r.Get(id)
	if !ok {
		return 0, 0, 0, 0, false
	}
	in, out = m.InputPer1M, m.OutputPer1M
	cachedIn, cacheWrite = m.CachedInputPer1M, m.CacheWritePer1M
	if cachedIn == 0 {
		cachedIn = in
	}
	if cacheWrite == 0 {
		cacheWrite = in
	}
	return in, cachedIn, cacheWrite, out, true
}

// CapabilitiesOf returns a model's capabilities (zero value = text-only for
// unknowns — the conservative default).
func (r *Registry) CapabilitiesOf(id string) Capabilities {
	m, _ := r.Get(id)
	return m.Capabilities
}

// Resolve maps an id-or-alias to its canonical id (the id itself if unknown).
func (r *Registry) Resolve(id string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolve(id)
}

// All returns every model, sorted by provider then id (deterministic API).
func (r *Registry) All() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(r.byID))
	for _, m := range r.byID {
		out = append(out, m)
	}
	sortModels(out, false)
	return out
}

// ByProvider returns a provider's models sorted by input price (cheapest
// first) — the order the modality redirect uses to pick a capable model.
func (r *Registry) ByProvider(provider string) []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Model
	for _, m := range r.byID {
		if m.Provider == provider {
			out = append(out, m)
		}
	}
	sortModels(out, true)
	return out
}

// Override adds or updates a model at runtime (config/DB-driven), layered on
// the embedded default. Concurrency-safe with hot-path reads.
func (r *Registry) Override(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.put(m)
}

// LoadOverrides applies a batch of overrides (e.g. parsed from a config file
// or DB) on top of the embedded default.
func (r *Registry) LoadOverrides(models []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range models {
		r.put(m)
	}
}

// DecodeOverrides decodes the operator override document — a JSON array of Model, the exact
// shape LENS_MODEL_CATALOG_OVERRIDES carries — into the batch LoadOverrides applies.
//
// ⚠ IT EXISTS BECAUSE `json.Unmarshal` INTO A FRESH Model MAKES "UNSAID" AND "FALSE" THE SAME BYTE,
// AND put REPLACES THE WHOLE ENTRY. cmd/lens advertises this variable as the way to "reprice models
// without a rebuild"; a reprice states a price and nothing else, so every FACT the catalog holds
// about that model — provider, capabilities, context window, aliases, display name — decoded as its
// zero value and overwrote the seeded truth. MEASURED through the proxy's real entry point, not
// inferred: `[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]` turned a 200 streaming
// vision request to gpt-4o into a 422 "model gpt-4o does not support it", dropped gpt-4o out of
// ByProvider("openai") (19 models → 18, so it stops anchoring that provider's fallback bound and
// stops being a redirect target), and emptied its capability entry in the introspection API. A price
// change silently withdrew a capability.
//
// So an override for an id the registry ALREADY holds is decoded ON TOP OF that entry: an absent
// field means "unchanged", a present one means what it says. `"capabilities":{"vision":false}` still
// turns vision off — the operator can still state a fact, they simply can no longer erase one by
// omission. An id the registry does not hold decodes from the zero Model exactly as before, because
// there is no truth to preserve and inventing one would be worse.
//
// ⚠ THE FOUR PRICE FIELDS ARE DELIBERATELY NOT PRESERVED, so this change moves NO money. They are
// zeroed before the decode, which leaves an omitted price reading exactly as it does today: unpriced,
// which ResolveRates routes to the loud fallback arm (see resolve.go#unpriced) rather than to a
// silent zero. Carrying the OLD rates forward would look tidier and would be a pricing decision —
// a model repriced 2.50 → 3.75 with cached_input_per_1m omitted would keep billing cache reads at
// the 1.25 that was 50% of the OLD list price and is 33% of the new one. That is a real question
// and it is not this function's to answer.
//
// ⚠ Lookup is by EXACT id, never through resolve(): an override written against a dated-snapshot
// ALIAS registers a new entry keyed by that alias, exactly as it does today. Merging an alias onto
// its canonical entry would rewrite the alias index as a side effect of a reprice, which is the
// class of silent consequence this function exists to remove.
func (r *Registry) DecodeOverrides(raw []byte) ([]Model, error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Model, 0, len(elems))
	for _, e := range elems {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(e, &probe); err != nil {
			return nil, err
		}
		base := r.byID[probe.ID] // zero Model when the id is new — the pre-existing behaviour
		base.InputPer1M, base.OutputPer1M = 0, 0
		base.CachedInputPer1M, base.CacheWritePer1M = 0, 0
		if err := json.Unmarshal(e, &base); err != nil {
			return nil, err
		}
		out = append(out, base)
	}
	return out, nil
}

func sortModels(ms []Model, byPrice bool) {
	sort.Slice(ms, func(i, j int) bool {
		if byPrice && ms[i].InputPer1M != ms[j].InputPer1M {
			return ms[i].InputPer1M < ms[j].InputPer1M
		}
		if !byPrice && ms[i].Provider != ms[j].Provider {
			return ms[i].Provider < ms[j].Provider
		}
		return ms[i].ID < ms[j].ID
	})
}

// ─── the global default catalog ───
// Package-level functions read this; the hot path (alerts pricing, modality
// capabilities) calls them directly.

var defaultRegistry = NewRegistry(seedModels())

func Get(id string) (Model, bool)                { return defaultRegistry.Get(id) }
func Price(id string) (in, out float64, ok bool) { return defaultRegistry.Price(id) }
func PriceDetailed(id string) (in, cachedIn, cacheWrite, out float64, ok bool) {
	return defaultRegistry.PriceDetailed(id)
}
func CapabilitiesOf(id string) Capabilities { return defaultRegistry.CapabilitiesOf(id) }
func Resolve(id string) string              { return defaultRegistry.Resolve(id) }
func All() []Model                          { return defaultRegistry.All() }
func ByProvider(provider string) []Model    { return defaultRegistry.ByProvider(provider) }
func Override(m Model)                      { defaultRegistry.Override(m) }
func LoadOverrides(models []Model)          { defaultRegistry.LoadOverrides(models) }
func DecodeOverrides(raw []byte) ([]Model, error) {
	return defaultRegistry.DecodeOverrides(raw)
}
