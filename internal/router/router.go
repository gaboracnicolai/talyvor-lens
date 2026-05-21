package router

import (
	"context"
	"strings"
)

type Router struct{}

type RoutingDecision struct {
	Provider string
	Model    string
	Reason   string
	CostTier string
}

type RequestComplexity struct {
	TokenEstimate  int
	HasCode        bool
	HasMath        bool
	HasMultiStep   bool
	IsCreative     bool
	RequiresReason bool
}

func New() *Router { return &Router{} }

// modelRank maps a known model name to its cost rank within its provider
// (0 = cheapest). Used by ShouldOverride to compare cost.
type modelInfo struct {
	provider string
	rank     int
}

var modelRanks = map[string]modelInfo{
	"gpt-4.1-nano":      {"openai", 0},
	"gpt-4o-mini":       {"openai", 1},
	"gpt-4o":            {"openai", 2},
	"claude-haiku-4-5":  {"anthropic", 0},
	"claude-sonnet-4-5": {"anthropic", 1},
	"claude-opus-4-5":   {"anthropic", 2},
}

var explicitCheapModels = map[string]struct{}{
	"gpt-4o-mini":      {},
	"claude-haiku-4-5": {},
	"gpt-4.1-nano":     {},
}

func (r *Router) Route(_ context.Context, provider, requestedModel, prompt string) RoutingDecision {
	if _, ok := explicitCheapModels[requestedModel]; ok {
		return RoutingDecision{
			Provider: provider,
			Model:    requestedModel,
			Reason:   "Explicit cheap model requested — respecting caller's choice",
			CostTier: "cheap",
		}
	}

	if provider != "openai" && provider != "anthropic" {
		return RoutingDecision{
			Provider: "openai",
			Model:    "gpt-4o-mini",
			Reason:   "Unknown provider — defaulted to openai gpt-4o-mini",
			CostTier: "cheap",
		}
	}

	score := AnalyseComplexity(prompt).Score()

	switch {
	case score <= 1:
		return cheap(provider)
	case score <= 3:
		return mid(provider)
	default:
		return premium(provider)
	}
}

func cheap(provider string) RoutingDecision {
	d := RoutingDecision{
		Provider: provider,
		Reason:   "Simple query — routed to cost-efficient model",
		CostTier: "cheap",
	}
	if provider == "anthropic" {
		d.Model = "claude-haiku-4-5"
	} else {
		d.Model = "gpt-4o-mini"
	}
	return d
}

func mid(provider string) RoutingDecision {
	d := RoutingDecision{
		Provider: provider,
		Reason:   "Moderate complexity — balanced model selected",
		CostTier: "mid",
	}
	if provider == "anthropic" {
		d.Model = "claude-sonnet-4-5"
	} else {
		d.Model = "gpt-4o"
	}
	return d
}

func premium(provider string) RoutingDecision {
	d := RoutingDecision{
		Provider: provider,
		Reason:   "High complexity — premium model required",
		CostTier: "premium",
	}
	if provider == "anthropic" {
		d.Model = "claude-opus-4-5"
	} else {
		d.Model = "gpt-4o"
	}
	return d
}

// ShouldOverride reports whether the routing decision should replace the
// caller's requestedModel. Only true when the decision picks a CHEAPER
// model in the same provider family — never upgrade silently.
func (r *Router) ShouldOverride(requestedModel string, decision RoutingDecision) bool {
	req, okReq := modelRanks[requestedModel]
	dec, okDec := modelRanks[decision.Model]
	if !okReq || !okDec || req.provider != dec.provider {
		return false
	}
	return dec.rank < req.rank
}

// AnalyseComplexity inspects the prompt for cost-relevant signals.
// Exported for tests and observability.
func AnalyseComplexity(prompt string) RequestComplexity {
	lower := strings.ToLower(prompt)
	return RequestComplexity{
		TokenEstimate:  len(prompt) / 4,
		HasCode:        containsAny(lower, "```", "func ", "def ", "class ", "import "),
		HasMath:        containsAny(lower, "∑", "∫", "equation", "calculate", "derive", "proof"),
		HasMultiStep:   containsAny(lower, "step by step", "first...then", "finally"),
		IsCreative:     containsAny(lower, "write a", "create a", "generate a", "story", "poem"),
		RequiresReason: containsAny(lower, "why", "explain", "reason", "compare", "analyse"),
	}
}

func (c RequestComplexity) Score() int {
	s := 0
	if c.TokenEstimate > 500 {
		s++
	}
	if c.HasCode {
		s++
	}
	if c.HasMath {
		s++
	}
	if c.HasMultiStep {
		s++
	}
	if c.IsCreative || c.RequiresReason {
		s++
	}
	return s
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
