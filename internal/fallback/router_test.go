package fallback

import (
	"errors"
	"testing"
)

func TestGetChain_DefaultForOpenAI(t *testing.T) {
	r := New()
	chain := r.GetChain("openai")
	if len(chain) < 2 {
		t.Fatalf("openai chain too short: %d", len(chain))
	}
	if chain[0].Provider != "anthropic" {
		t.Errorf("openai fallback[0] provider = %q, want anthropic", chain[0].Provider)
	}
	if chain[0].Model != "claude-sonnet-4-6" {
		t.Errorf("openai fallback[0] model = %q, want claude-sonnet-4-6", chain[0].Model)
	}
	if chain[1].Provider != "google" {
		t.Errorf("openai fallback[1] provider = %q, want google", chain[1].Provider)
	}
	if chain[1].Model != "gemini-2.5-flash" {
		t.Errorf("openai fallback[1] model = %q, want gemini-2.5-flash", chain[1].Model)
	}
}

func TestGetChain_DefaultForAnthropic(t *testing.T) {
	r := New()
	chain := r.GetChain("anthropic")
	if len(chain) < 2 {
		t.Fatalf("anthropic chain too short: %d", len(chain))
	}
	if chain[0].Provider != "openai" || chain[0].Model != "gpt-4o" {
		t.Errorf("anthropic fallback[0] = %+v, want openai/gpt-4o", chain[0])
	}
	if chain[1].Provider != "google" || chain[1].Model != "gemini-2.5-flash" {
		t.Errorf("anthropic fallback[1] = %+v, want google/gemini-2.5-flash", chain[1])
	}
}

func TestGetChain_DefaultForGoogle(t *testing.T) {
	r := New()
	chain := r.GetChain("google")
	if len(chain) < 2 {
		t.Fatalf("google chain too short: %d", len(chain))
	}
	if chain[0].Provider != "openai" || chain[0].Model != "gpt-4o" {
		t.Errorf("google fallback[0] = %+v, want openai/gpt-4o", chain[0])
	}
	if chain[1].Provider != "anthropic" || chain[1].Model != "claude-sonnet-4-6" {
		t.Errorf("google fallback[1] = %+v, want anthropic/claude-sonnet-4-6", chain[1])
	}
}

func TestSetChain_OverridesDefault(t *testing.T) {
	r := New()
	custom := []FallbackTarget{
		{Provider: "google", Model: "gemini-2.5-flash", Priority: 1},
	}
	r.SetChain("openai", custom)
	chain := r.GetChain("openai")
	if len(chain) != 1 {
		t.Fatalf("custom chain length = %d, want 1", len(chain))
	}
	if chain[0].Provider != "google" {
		t.Errorf("custom chain[0] = %+v, want google/gemini-2.5-flash", chain[0])
	}
}

func TestShouldFallback_TrueFor500Status(t *testing.T) {
	r := New()
	if !r.ShouldFallback(500, nil) {
		t.Error("ShouldFallback(500, nil) = false, want true")
	}
}

func TestShouldFallback_TrueFor429Status(t *testing.T) {
	r := New()
	if !r.ShouldFallback(429, nil) {
		t.Error("ShouldFallback(429, nil) = false, want true")
	}
}

func TestShouldFallback_TrueForNetworkError(t *testing.T) {
	r := New()
	if !r.ShouldFallback(0, errors.New("connection reset")) {
		t.Error("ShouldFallback(0, network err) = false, want true")
	}
}

func TestShouldFallback_FalseFor200(t *testing.T) {
	r := New()
	if r.ShouldFallback(200, nil) {
		t.Error("ShouldFallback(200, nil) = true, want false")
	}
}

func TestShouldFallback_FalseFor400ClientError(t *testing.T) {
	r := New()
	if r.ShouldFallback(400, nil) {
		t.Error("ShouldFallback(400, nil) = true, want false (4xx client errors are not fallbackable except 429)")
	}
	if r.ShouldFallback(404, nil) {
		t.Error("ShouldFallback(404, nil) = true, want false")
	}
}
