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

// modelRanks maps each model name to its cost rank within the provider
// family (0 = cheapest). Used by ShouldOverride to decide whether a
// routing decision is a strict cost reduction.
var modelRanks = map[string]modelInfo{
	// OpenAI, cheapest first.
	"gpt-4.1-nano":  {"openai", 0},
	"gpt-4o-mini":   {"openai", 1},
	"gpt-4.1-mini":  {"openai", 2},
	"gpt-4.1":       {"openai", 3},
	"gpt-4o":        {"openai", 4},
	"gpt-5.4-mini":  {"openai", 5},
	"gpt-5.4":       {"openai", 6},
	// Anthropic, cheapest first.
	"claude-haiku-4-5":  {"anthropic", 0},
	"claude-haiku-4-6":  {"anthropic", 1},
	"claude-sonnet-4-5": {"anthropic", 2},
	"claude-sonnet-4-6": {"anthropic", 3},
	"claude-opus-4-5":   {"anthropic", 4},
	"claude-opus-4-6":   {"anthropic", 5},
	// Google, cheapest first.
	"gemini-1.5-flash": {"google", 0},
	"gemini-2.0-flash": {"google", 1},
	"gemini-2.5-flash": {"google", 2},
	"gemini-1.5-pro":   {"google", 3},
	"gemini-2.5-pro":   {"google", 4},
}

// explicitCheapModels is the fast-path set for "the caller asked for a
// cheap model on purpose, leave their choice alone." Includes every
// mini / nano / haiku variant.
var explicitCheapModels = map[string]struct{}{
	"gpt-4o-mini":      {},
	"gpt-4.1-nano":     {},
	"gpt-4.1-mini":     {},
	"gpt-5.4-mini":     {},
	"claude-haiku-4-5": {},
	"claude-haiku-4-6": {},
	"gemini-2.5-flash": {},
	"gemini-2.0-flash": {},
	"gemini-1.5-flash": {},
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

	if provider != "openai" && provider != "anthropic" && provider != "google" {
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
	switch provider {
	case "anthropic":
		d.Model = "claude-haiku-4-6"
	case "google":
		d.Model = "gemini-2.5-flash"
	default:
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
	switch provider {
	case "anthropic":
		d.Model = "claude-sonnet-4-6"
	case "google":
		// No distinct mid tier on Gemini yet — flash handles it.
		d.Model = "gemini-2.5-flash"
	default:
		d.Model = "gpt-4.1"
	}
	return d
}

func premium(provider string) RoutingDecision {
	d := RoutingDecision{
		Provider: provider,
		Reason:   "High complexity — premium model required",
		CostTier: "premium",
	}
	switch provider {
	case "anthropic":
		d.Model = "claude-opus-4-6"
	case "google":
		d.Model = "gemini-2.5-pro"
	default:
		d.Model = "gpt-5.4"
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
