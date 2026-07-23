package catalog

import "testing"

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestPriceDetailed_CacheAwareRates: the catalog carries per-provider prompt-cache
// rates so the cost basis can price cache reads/writes correctly.
//   - Anthropic (verified live docs): cache read 0.1x input, 5-min cache write 1.25x input.
//   - OpenAI (published GPT-4o-gen): cache read ~0.5x input; no separate write charge pre-5.6.
//   - Other providers (google/mistral/groq): caching not billed through this path → no discount
//     (cache read == input rate), the conservative choice that never under-bills.
func TestPriceDetailed_CacheAwareRates(t *testing.T) {
	cases := []struct {
		id                            string
		in, cachedIn, cacheWrite, out float64
		ok                            bool
	}{
		{"claude-sonnet-4-5", 3.00, 0.30, 3.75, 15.00, true}, // 0.1x read, 1.25x write
		{"claude-opus-4-5", 15.00, 1.50, 18.75, 75.00, true},
		{"gpt-4o", 2.50, 1.25, 2.50, 10.00, true}, // 0.5x read, 1.0x (no write charge)
		{"gemini-2.5-pro", 1.25, 1.25, 1.25, 10.00, true}, // no discount (not billed here)
		{"unknown-model", 0, 0, 0, 0, false},
	}
	for _, tc := range cases {
		in, cachedIn, cacheWrite, out, ok := PriceDetailed(tc.id)
		if ok != tc.ok {
			t.Errorf("%s: ok=%v want %v", tc.id, ok, tc.ok)
			continue
		}
		if !approxEq(in, tc.in) || !approxEq(cachedIn, tc.cachedIn) || !approxEq(cacheWrite, tc.cacheWrite) || !approxEq(out, tc.out) {
			t.Errorf("%s: PriceDetailed=(in %v cachedIn %v cacheWrite %v out %v), want (%v %v %v %v)",
				tc.id, in, cachedIn, cacheWrite, out, tc.in, tc.cachedIn, tc.cacheWrite, tc.out)
		}
	}
}

// TestPriceDetailed_MatchesPriceForInputOutput: PriceDetailed must return the SAME
// input/output rates as Price for every seeded model — the additive breakdown must
// never perturb the existing price basis the cost moat depends on.
func TestPriceDetailed_MatchesPriceForInputOutput(t *testing.T) {
	for _, m := range All() {
		in, out, ok := Price(m.ID)
		dIn, _, _, dOut, dOK := PriceDetailed(m.ID)
		if ok != dOK || !approxEq(in, dIn) || !approxEq(out, dOut) {
			t.Errorf("%s: Price=(%v,%v,%v) but PriceDetailed input/output=(%v,%v,%v)", m.ID, in, out, ok, dIn, dOut, dOK)
		}
	}
}

// TestPriceDetailed_OverrideWithoutCacheRatesFallsBackToInput: an operator override
// that sets only input/output (no cache rates) must bill cache read/write tokens at
// the INPUT rate — never free (0). Conservative: never under-bills a cached token.
func TestPriceDetailed_OverrideWithoutCacheRatesFallsBackToInput(t *testing.T) {
	r := NewRegistry(nil)
	r.Override(Model{ID: "custom-x", Provider: "openai", InputPer1M: 4.00, OutputPer1M: 8.00})
	in, cachedIn, cacheWrite, out, ok := r.PriceDetailed("custom-x")
	if !ok || !approxEq(in, 4.00) || !approxEq(cachedIn, 4.00) || !approxEq(cacheWrite, 4.00) || !approxEq(out, 8.00) {
		t.Fatalf("override fallback: got (in %v cachedIn %v cacheWrite %v out %v ok %v), want cached/write == input 4.00", in, cachedIn, cacheWrite, out, ok)
	}
}
