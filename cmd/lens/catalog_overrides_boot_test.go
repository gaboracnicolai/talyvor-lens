package main

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// THE BOOT PATH ITSELF, DRIVEN FROM THE ENV STRING AN OPERATOR WOULD ACTUALLY SET.
//
// internal/catalog pins what DecodeOverrides does and internal/proxy pins what the customer sees.
// Neither can tell you that cmd/lens calls it — and #157's lesson in this repo is that the closure
// is the WIRING: a rule nothing executes is a rule nothing can red. So this drives the real boot
// function with a real LENS_MODEL_CATALOG_OVERRIDES document.
func TestApplyCatalogOverrides_RepriceDoesNotBlankTheModel(t *testing.T) {
	seeded, ok := catalog.Get("gpt-4o")
	if !ok || !seeded.Capabilities.Vision || seeded.Provider != "openai" {
		t.Fatalf("CONTROL: seeded gpt-4o = %+v ok=%v — this test is measuring nothing", seeded, ok)
	}
	t.Cleanup(func() { catalog.Override(seeded) })

	// Exactly what an operator following main.go's own documentation would export.
	applyCatalogOverrides(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`, quietLogger())

	m, ok := catalog.Get("gpt-4o")
	if !ok {
		t.Fatal("gpt-4o vanished from the catalog at boot")
	}
	if m.InputPer1M != 3.75 || m.OutputPer1M != 15.00 {
		t.Fatalf("the reprice did not apply (%v/%v) — every assertion below would pass vacuously", m.InputPer1M, m.OutputPer1M)
	}
	if !m.Capabilities.Vision {
		t.Error("boot blanked gpt-4o's vision capability — a price change withdrew a capability")
	}
	if m.Provider != "openai" {
		t.Errorf("boot blanked the provider (%q) — the model drops out of its own fallback anchor set (fallbackRates filters on m.Provider)", m.Provider)
	}
	if m.ContextTokens != seeded.ContextTokens || len(m.Aliases) != len(seeded.Aliases) {
		t.Errorf("boot blanked context/aliases: %d/%v, want %d/%v", m.ContextTokens, m.Aliases, seeded.ContextTokens, seeded.Aliases)
	}
}

// An unparseable document must be refused whole, leaving the catalog exactly as it was — never
// half-applied, and never a reason not to boot.
func TestApplyCatalogOverrides_InvalidDocumentChangesNothing(t *testing.T) {
	before, ok := catalog.Get("gpt-4o")
	if !ok {
		t.Fatal("no gpt-4o to compare against")
	}
	t.Cleanup(func() { catalog.Override(before) })

	applyCatalogOverrides(`{"id":"gpt-4o"}`, quietLogger()) // an object, not the documented array
	after, _ := catalog.Get("gpt-4o")
	if after.InputPer1M != before.InputPer1M || after.Capabilities != before.Capabilities {
		t.Errorf("an invalid override document mutated the catalog: %+v -> %+v", before, after)
	}

	applyCatalogOverrides("", quietLogger()) // unset
	if empty, _ := catalog.Get("gpt-4o"); empty.InputPer1M != before.InputPer1M {
		t.Error("an EMPTY LENS_MODEL_CATALOG_OVERRIDES mutated the catalog")
	}
}

// And main() must actually hand the variable to it. The behaviour tests above run the function; this
// is the one claim they cannot make — that boot reaches it. Same shape as
// internal/poolroyalty/sweeper_finaltype_guard_test.go, which pins sweeper wiring out of this file.
var catalogOverrideWiring = regexp.MustCompile(`applyCatalogOverrides\(os\.Getenv\("LENS_MODEL_CATALOG_OVERRIDES"\)`)

func TestMain_WiresCatalogOverridesThroughTheMergingDecoder(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !catalogOverrideWiring.Match(src) {
		t.Error("cmd/lens/main.go no longer passes LENS_MODEL_CATALOG_OVERRIDES to applyCatalogOverrides — " +
			"if it decodes the document itself again, a price-only override goes back to blanking every " +
			"fact it did not restate and the tests in internal/catalog cannot see it")
	}
	if regexp.MustCompile(`json\.Unmarshal\(\[\]byte\(raw\), &overrides\)`).Match(src) {
		t.Error("cmd/lens/main.go decodes the override document with a bare json.Unmarshal again — " +
			"that is the decode that made \"unsaid\" and \"false\" the same byte")
	}
}
