// Package fallback decides — and caches — which alternate provider
// Lens should try when the primary fails. The router itself is pure:
// no HTTP, no DB. It owns the policy (which provider+model is next)
// and the decision (does this status code / error warrant a fallback).
// The proxy package owns the actual retry orchestration.
package fallback

import "sync"

type FallbackTarget struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
}

type FallbackResult struct {
	UsedProvider string `json:"used_provider"`
	UsedModel    string `json:"used_model"`
	Attempts     int    `json:"attempts"`
	FellBack     bool   `json:"fell_back"`
}

type FallbackRouter struct {
	mu     sync.RWMutex
	chains map[string][]FallbackTarget
}

// defaultChains encodes the cross-provider fallback policy. Order is
// "next cheapest comparable model": Sonnet ≈ GPT-4o; Gemini Flash is
// the cheap last-resort that any provider can fall through to.
func defaultChains() map[string][]FallbackTarget {
	return map[string][]FallbackTarget{
		"openai": {
			{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 1},
			{Provider: "google", Model: "gemini-2.5-flash", Priority: 2},
		},
		"anthropic": {
			{Provider: "openai", Model: "gpt-4o", Priority: 1},
			{Provider: "google", Model: "gemini-2.5-flash", Priority: 2},
		},
		"google": {
			{Provider: "openai", Model: "gpt-4o", Priority: 1},
			{Provider: "anthropic", Model: "claude-sonnet-4-6", Priority: 2},
		},
	}
}

func New() *FallbackRouter {
	return &FallbackRouter{chains: defaultChains()}
}

// GetChain returns a defensive copy of the chain so callers can't
// mutate router state by reslicing the result.
func (r *FallbackRouter) GetChain(provider string) []FallbackTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.chains[provider]
	if len(src) == 0 {
		return nil
	}
	out := make([]FallbackTarget, len(src))
	copy(out, src)
	return out
}

func (r *FallbackRouter) SetChain(provider string, targets []FallbackTarget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.chains == nil {
		r.chains = make(map[string][]FallbackTarget)
	}
	stored := make([]FallbackTarget, len(targets))
	copy(stored, targets)
	r.chains[provider] = stored
}

// AllChains returns a snapshot of every configured chain. Used by the
// /v1/api/fallback/chains endpoint.
func (r *FallbackRouter) AllChains() map[string][]FallbackTarget {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string][]FallbackTarget, len(r.chains))
	for k, v := range r.chains {
		cp := make([]FallbackTarget, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// ShouldFallback decides whether an upstream attempt counts as
// failed-and-should-try-the-next-provider. 4xx is the client's problem
// (bad request, missing auth, schema error) and would fail the same
// way against any provider — only 429 escapes since it indicates
// quota exhaustion that another provider doesn't share.
func (r *FallbackRouter) ShouldFallback(statusCode int, err error) bool {
	if err != nil {
		return true
	}
	if statusCode >= 500 {
		return true
	}
	if statusCode == 429 {
		return true
	}
	return false
}
