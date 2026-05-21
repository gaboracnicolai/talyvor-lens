package router

import (
	"context"
	"strings"
	"testing"
)

func TestRoute_SimplePromptCheapOpenAI(t *testing.T) {
	r := New()
	got := r.Route(context.Background(), "openai", "gpt-4o", "hello")

	if got.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", got.Provider, "openai")
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-4o-mini")
	}
	if got.CostTier != "cheap" {
		t.Errorf("CostTier = %q, want %q", got.CostTier, "cheap")
	}
	if got.Reason == "" {
		t.Error("Reason should not be empty")
	}
}

func TestRoute_SimplePromptCheapAnthropic(t *testing.T) {
	r := New()
	got := r.Route(context.Background(), "anthropic", "claude-opus-4-5", "hi there")

	if got.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", got.Provider, "anthropic")
	}
	if got.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-haiku-4-5")
	}
	if got.CostTier != "cheap" {
		t.Errorf("CostTier = %q, want %q", got.CostTier, "cheap")
	}
}

func TestRoute_CodeHeavyPromptMidTier(t *testing.T) {
	r := New()
	// HasCode (1) + TokenEstimate>500 from length (1) = score 2 → mid
	prompt := "```python\n" + strings.Repeat("def f(): pass\n", 200) + "\n```"
	got := r.Route(context.Background(), "openai", "gpt-4o", prompt)

	if got.CostTier != "mid" {
		t.Errorf("CostTier = %q, want %q (decision: %+v)", got.CostTier, "mid", got)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-4o")
	}
}

func TestRoute_ComplexPromptPremiumTier(t *testing.T) {
	r := New()
	// HasCode + HasMath + HasMultiStep + RequiresReason + long → score 5 → premium
	prompt := "Calculate the integral step by step and explain why this derivation works. " +
		"Then show the proof in code:\n```python\n" +
		strings.Repeat("def proof(): return True\n", 100) +
		"\n```\nFinally, compare this approach to alternatives."

	got := r.Route(context.Background(), "anthropic", "claude-sonnet-4-5", prompt)

	if got.CostTier != "premium" {
		t.Errorf("CostTier = %q, want %q (decision: %+v)", got.CostTier, "premium", got)
	}
	if got.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", got.Provider, "anthropic")
	}
	if got.Model != "claude-opus-4-5" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-opus-4-5")
	}
}

func TestRoute_ExplicitCheapModelRespected(t *testing.T) {
	r := New()
	// Complex prompt that would otherwise route premium
	prompt := "Calculate step by step and explain why with ```code``` proof"

	for _, model := range []string{"gpt-4o-mini", "claude-haiku-4-5", "gpt-4.1-nano"} {
		t.Run(model, func(t *testing.T) {
			got := r.Route(context.Background(), "openai", model, prompt)
			if got.Model != model {
				t.Errorf("Model = %q, want %q (explicit cheap model should be respected)", got.Model, model)
			}
			if got.CostTier != "cheap" {
				t.Errorf("CostTier = %q, want %q", got.CostTier, "cheap")
			}
		})
	}
}

func TestRoute_UnknownProviderDefaultsToOpenAI(t *testing.T) {
	r := New()
	got := r.Route(context.Background(), "cohere", "command-r", "hello")

	if got.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", got.Provider, "openai")
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-4o-mini")
	}
}

func TestShouldOverride_FalseWhenSameOrMoreExpensive(t *testing.T) {
	r := New()

	cases := []struct {
		name      string
		requested string
		decision  RoutingDecision
	}{
		{"same model openai", "gpt-4o", RoutingDecision{Provider: "openai", Model: "gpt-4o"}},
		{"more expensive openai", "gpt-4o-mini", RoutingDecision{Provider: "openai", Model: "gpt-4o"}},
		{"same model anthropic", "claude-sonnet-4-5", RoutingDecision{Provider: "anthropic", Model: "claude-sonnet-4-5"}},
		{"more expensive anthropic", "claude-haiku-4-5", RoutingDecision{Provider: "anthropic", Model: "claude-opus-4-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r.ShouldOverride(tc.requested, tc.decision) {
				t.Errorf("ShouldOverride(%q, %+v) = true, want false", tc.requested, tc.decision)
			}
		})
	}
}

func TestShouldOverride_TrueWhenCheaper(t *testing.T) {
	r := New()

	cases := []struct {
		name      string
		requested string
		decision  RoutingDecision
	}{
		{"openai downgrade to mini", "gpt-4o", RoutingDecision{Provider: "openai", Model: "gpt-4o-mini"}},
		{"openai downgrade to nano", "gpt-4o", RoutingDecision{Provider: "openai", Model: "gpt-4.1-nano"}},
		{"anthropic downgrade to sonnet", "claude-opus-4-5", RoutingDecision{Provider: "anthropic", Model: "claude-sonnet-4-5"}},
		{"anthropic downgrade to haiku", "claude-opus-4-5", RoutingDecision{Provider: "anthropic", Model: "claude-haiku-4-5"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !r.ShouldOverride(tc.requested, tc.decision) {
				t.Errorf("ShouldOverride(%q, %+v) = false, want true", tc.requested, tc.decision)
			}
		})
	}
}
