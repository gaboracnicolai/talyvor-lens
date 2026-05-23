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
	if got.Model != "claude-haiku-4-6" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-haiku-4-6")
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
	if got.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-4.1")
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
	if got.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-opus-4-6")
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

func TestRoute_PremiumTierOpenAIReturnsGPT54(t *testing.T) {
	r := New()
	// Pile on enough signals to push the score into the premium tier.
	prompt := "Calculate the integral step by step and explain why this derivation works. " +
		"Show the proof in code:\n```python\n" +
		strings.Repeat("def proof(): return True\n", 100) +
		"\n```\nFinally, compare this approach to alternatives."

	got := r.Route(context.Background(), "openai", "gpt-4o", prompt)
	if got.CostTier != "premium" {
		t.Fatalf("CostTier = %q, want premium (decision: %+v)", got.CostTier, got)
	}
	if got.Model != "gpt-5.4" {
		t.Errorf("Model = %q, want gpt-5.4", got.Model)
	}
}

func TestRoute_PremiumTierAnthropicReturnsClaudeOpus46(t *testing.T) {
	r := New()
	prompt := "Calculate step by step and explain why this works in detail. " +
		"Provide a thorough proof:\n```python\n" +
		strings.Repeat("def proof(): return True\n", 100) +
		"\n```\nCompare with alternative approaches."

	got := r.Route(context.Background(), "anthropic", "claude-sonnet-4-5", prompt)
	if got.CostTier != "premium" {
		t.Fatalf("CostTier = %q, want premium (decision: %+v)", got.CostTier, got)
	}
	if got.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want claude-opus-4-6", got.Model)
	}
}

func TestRoute_ClaudeHaiku46RecognisedAsCheap(t *testing.T) {
	r := New()
	// Premium-looking prompt; an explicit cheap-model request must still
	// win, proving claude-haiku-4-6 is in the explicit-cheap set.
	prompt := "Calculate step by step and explain why with ```code``` proof"

	got := r.Route(context.Background(), "anthropic", "claude-haiku-4-6", prompt)
	if got.Model != "claude-haiku-4-6" {
		t.Errorf("Model = %q, want claude-haiku-4-6 (explicit cheap model should be respected)", got.Model)
	}
	if got.CostTier != "cheap" {
		t.Errorf("CostTier = %q, want cheap", got.CostTier)
	}
}

func TestRoute_MistralCheapTier(t *testing.T) {
	r := New()
	got := r.Route(context.Background(), "mistral", "mistral-large-latest", "hi")

	if got.Provider != "mistral" {
		t.Errorf("Provider = %q, want mistral", got.Provider)
	}
	if got.Model != "mistral-small-latest" {
		t.Errorf("cheap-tier model = %q, want mistral-small-latest", got.Model)
	}
	if got.CostTier != "cheap" {
		t.Errorf("CostTier = %q, want cheap", got.CostTier)
	}
}

func TestRoute_MistralPremiumTier(t *testing.T) {
	r := New()
	// Same prompt shape that drives TestRoute_ComplexPromptPremiumTier:
	// code + math + multi-step + reasoning words → score > 3.
	prompt := "Calculate the integral step by step and explain why this derivation works. " +
		"Then show the proof in code:\n```python\n" +
		strings.Repeat("def proof(): return True\n", 100) +
		"\n```\nFinally, compare this approach to alternatives."

	got := r.Route(context.Background(), "mistral", "mistral-large-latest", prompt)
	if got.Model != "mistral-large-latest" {
		t.Errorf("premium-tier mistral model = %q, want mistral-large-latest", got.Model)
	}
	if got.CostTier != "premium" {
		t.Errorf("CostTier = %q, want premium", got.CostTier)
	}
}

func TestRoute_GroqCheapAndMidTiers(t *testing.T) {
	r := New()
	// Cheap branch.
	cheap := r.Route(context.Background(), "groq", "llama-3.3-70b-versatile", "hi")
	if cheap.Model != "llama-3.1-8b-instant" {
		t.Errorf("cheap-tier groq model = %q, want llama-3.1-8b-instant", cheap.Model)
	}

	// Mid branch — needs a complexity score of 2 or 3.
	midPrompt := "```python\n" + strings.Repeat("def f(): pass\n", 200) + "\n```"
	mid := r.Route(context.Background(), "groq", "llama-3.3-70b-versatile", midPrompt)
	if mid.Model != "llama-3.3-70b-versatile" {
		t.Errorf("mid-tier groq model = %q, want llama-3.3-70b-versatile", mid.Model)
	}
}

func TestRoute_VLLMPreservesRequestedModel(t *testing.T) {
	r := New()
	// vLLM must NEVER override the caller's model — the alternative
	// probably isn't loaded on the endpoint.
	got := r.Route(context.Background(), "vllm", "vllm/llama-3-private", "hi")
	if got.Provider != "vllm" {
		t.Errorf("Provider = %q, want vllm", got.Provider)
	}
	if got.Model != "vllm/llama-3-private" {
		t.Errorf("vLLM should preserve requestedModel; got %q", got.Model)
	}
	if got.CostTier != "passthrough" {
		t.Errorf("CostTier = %q, want passthrough", got.CostTier)
	}
}
