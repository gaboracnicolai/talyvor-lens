package catalog

import (
	"sync"
	"testing"
)

// previousPrices is the EXACT price table that lived in alerts.modelPrices
// before this consolidation — an independent golden reference. The catalog
// must price every one of these models identically. A diff here means a
// silent re-pricing, which would corrupt every budget/forecast/anomaly/ROI
// figure. This is the critical gate.
var previousPrices = map[string][2]float64{
	"gpt-4o":            {2.50, 10.00},
	"gpt-4o-mini":       {0.15, 0.60},
	"gpt-4.1-nano":      {0.10, 0.40},
	"gpt-5.4":           {5.00, 20.00},
	"gpt-5.4-mini":      {0.50, 2.00},
	"gpt-4.1":           {2.00, 8.00},
	"gpt-4.1-mini":      {0.40, 1.60},
	"claude-opus-4-5":   {15.00, 75.00},
	"claude-sonnet-4-5": {3.00, 15.00},
	"claude-haiku-4-5":  {0.80, 4.00},
	"claude-opus-4-6":   {15.00, 75.00},
	"claude-sonnet-4-6": {3.00, 15.00},
	"gemini-2.5-pro":    {1.25, 10.00},
	"gemini-2.5-flash":  {0.075, 0.30},
	"gemini-2.0-flash":  {0.10, 0.40},
	"gemini-1.5-pro":    {1.25, 5.00},
	"gemini-1.5-flash":  {0.075, 0.30},
	"anthropic.claude-opus-4-6-20251101-v1:0":   {17.25, 86.25},
	"anthropic.claude-sonnet-4-6-20251101-v1:0": {3.45, 17.25},
	"mistral-large-latest":                      {2.00, 6.00},
	"mistral-small-latest":                      {0.10, 0.30},
	"mistral-nemo":                              {0.015, 0.045},
	"open-mistral-7b":                           {0.025, 0.025},
	"llama-3.3-70b-versatile":                   {0.59, 0.79},
	"llama-3.1-8b-instant":                      {0.05, 0.08},
	"mixtral-8x7b-32768":                        {0.24, 0.24},
	"gemma2-9b-it":                              {0.20, 0.20},
}

func TestPriceParity_NoSilentRepricing(t *testing.T) {
	for id, want := range previousPrices {
		in, out, ok := Price(id)
		if !ok {
			t.Errorf("%s: missing from catalog (was priced before)", id)
			continue
		}
		if in != want[0] || out != want[1] {
			t.Errorf("%s: catalog price (%.4f/%.4f) != previous (%.4f/%.4f) — SILENT RE-PRICING", id, in, out, want[0], want[1])
		}
	}
}

// previousCapabilities is the exact capability registry from
// modality.capabilities before consolidation. (vision, audio, document)
var previousCapabilities = map[string][3]bool{
	"gpt-4o":            {true, false, false},
	"gpt-4o-mini":       {true, false, false},
	"gpt-4.1":           {true, false, false},
	"gpt-4.1-mini":      {true, false, false},
	"gpt-4.1-nano":      {true, false, false},
	"gpt-5.4":           {true, false, false},
	"gpt-5.4-mini":      {true, false, false},
	"claude-opus-4-5":   {true, false, true},
	"claude-opus-4-6":   {true, false, true},
	"claude-sonnet-4-5": {true, false, true},
	"claude-sonnet-4-6": {true, false, true},
	"claude-haiku-4-5":  {true, false, true},
	"gemini-1.5-pro":    {true, true, true},
	"gemini-1.5-flash":  {true, true, true},
	"gemini-2.0-flash":  {true, true, true},
	"gemini-2.5-flash":  {true, true, true},
	"gemini-2.5-pro":    {true, true, true},
	"anthropic.claude-opus-4-6-20251101-v1:0":   {true, false, true},
	"anthropic.claude-sonnet-4-6-20251101-v1:0": {true, false, true},
}

func TestCapabilityParity(t *testing.T) {
	for id, want := range previousCapabilities {
		c := CapabilitiesOf(id)
		if c.Vision != want[0] || c.Audio != want[1] || c.Document != want[2] {
			t.Errorf("%s: catalog caps (%v/%v/%v) != previous (%v/%v/%v)", id, c.Vision, c.Audio, c.Document, want[0], want[1], want[2])
		}
	}
}

func TestUnknownModel_PricesZero(t *testing.T) {
	if _, _, ok := Price("totally-made-up-model"); ok {
		t.Fatal("an unknown model must not be in the catalog (cost lookup → 0, as before)")
	}
	c := CapabilitiesOf("totally-made-up-model")
	if c.Vision || c.Audio || c.Document {
		t.Fatal("unknown model must be text-only (conservative)")
	}
}

func TestAliasResolution(t *testing.T) {
	// A dated snapshot resolves to its canonical model + prices.
	m, ok := Get("gpt-4o-2024-11-20")
	if !ok || m.ID != "gpt-4o" {
		t.Fatalf("dated snapshot should resolve to gpt-4o: %+v ok=%v", m, ok)
	}
	in, out, _ := Price("gpt-4o-2024-08-06")
	if in != 2.50 || out != 10.00 {
		t.Fatalf("aliased snapshot must price as gpt-4o: %.2f/%.2f", in, out)
	}
	if Resolve("gpt-4o-mini-2024-07-18") != "gpt-4o-mini" {
		t.Fatal("alias must resolve to its canonical id")
	}
}

func TestOpus48_PresentSelectableVerifiedPricing(t *testing.T) {
	m, ok := Get("claude-opus-4-8")
	if !ok {
		t.Fatal("claude-opus-4-8 must be in the catalog (selectable)")
	}
	if m.InputPer1M != 5.00 || m.OutputPer1M != 25.00 {
		t.Fatalf("Opus 4.8 pricing must be $5/$25: got %.2f/%.2f", m.InputPer1M, m.OutputPer1M)
	}
	if !m.Capabilities.Vision {
		t.Fatal("Opus 4.8 must be vision-capable")
	}
	if m.Provider != "anthropic" {
		t.Fatalf("Opus 4.8 provider: got %q want anthropic", m.Provider)
	}
	if m.ContextTokens != 200000 {
		t.Fatalf("Opus 4.8 context: got %d want 200000", m.ContextTokens)
	}
}

func TestRuntimeOverride_AddsAndUpdatesWithoutRebuild(t *testing.T) {
	r := NewRegistry(seedModels()) // local registry; don't mutate the global

	// Add a brand-new model.
	r.Override(Model{ID: "new-model-x", Provider: "openai", InputPer1M: 1.11, OutputPer1M: 2.22, Capabilities: Capabilities{Vision: true}})
	if in, out, ok := r.Price("new-model-x"); !ok || in != 1.11 || out != 2.22 {
		t.Fatalf("override should add a model: %.2f/%.2f ok=%v", in, out, ok)
	}

	// Reprice an existing model (operator correction without a rebuild).
	r.Override(Model{ID: "gpt-4o", Provider: "openai", InputPer1M: 9.99, OutputPer1M: 19.99})
	if in, _, _ := r.Price("gpt-4o"); in != 9.99 {
		t.Fatalf("override should update an existing model: got %.2f", in)
	}
	// The global default is unaffected by the local override.
	if in, _, _ := Price("gpt-4o"); in != 2.50 {
		t.Fatalf("global catalog must be unchanged: got %.2f", in)
	}
}

func TestConcurrentReadsDuringOverride(t *testing.T) {
	r := NewRegistry(seedModels())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				_, _, _ = r.Price("gpt-4o")
				_ = r.CapabilitiesOf("claude-opus-4-8")
				_, _ = r.Get("gemini-2.5-pro")
			}
		}()
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.Override(Model{ID: "churn", Provider: "openai", InputPer1M: float64(n)})
			}
		}(w)
	}
	wg.Wait()
}
