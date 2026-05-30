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
type Model struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	DisplayName   string       `json:"display_name"`
	InputPer1M    float64      `json:"input_per_1m"`
	OutputPer1M   float64      `json:"output_per_1m"`
	Capabilities  Capabilities `json:"capabilities"`
	ContextTokens int          `json:"context_tokens"`
	MaxOutput     int          `json:"max_output"`
	Deprecated    bool         `json:"deprecated,omitempty"`
	Aliases       []string     `json:"aliases,omitempty"` // e.g. dated snapshots → this canonical id
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
func CapabilitiesOf(id string) Capabilities      { return defaultRegistry.CapabilitiesOf(id) }
func Resolve(id string) string                   { return defaultRegistry.Resolve(id) }
func All() []Model                               { return defaultRegistry.All() }
func ByProvider(provider string) []Model         { return defaultRegistry.ByProvider(provider) }
func Override(m Model)                           { defaultRegistry.Override(m) }
func LoadOverrides(models []Model)               { defaultRegistry.LoadOverrides(models) }
