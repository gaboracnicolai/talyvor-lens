package modelwatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
)

// THE DETECTOR AND THE BILL WENT BLIND ON THE SAME BYTE.
//
// Check's own comment explains why it asks ResolveRates rather than reading the catalog data: an operator
// who has correctly priced a model on the box must not be re-alerted hourly. That reasoning is right, and
// it inherits whatever ResolveRates means by "priced". While a registered-but-priceless model resolved as
// `exact`, this detector — the PREVENTIVE half of the unpriced-model fix, the thing that is supposed to
// find the hole before a customer's request does — skipped the one model in the list being served free.
//
// So the two ends failed together: the bill said $0.00 and reported the price as exact, and the detector
// whose job is to notice unpriceable models agreed there was nothing to report.
func TestCheck_APricelessOverrideIsDriftNotCoverage(t *testing.T) {
	const priceless = "gpt-priceless-4m8k"

	// Registered the way an operator registers it, with the price fields absent.
	var overrides []catalog.Model
	if err := json.Unmarshal([]byte(`[{"id":"`+priceless+`","provider":"openai"}]`), &overrides); err != nil {
		t.Fatalf("override JSON did not decode: %v", err)
	}
	catalog.LoadOverrides(overrides)
	if _, ok := catalog.Get(priceless); !ok {
		t.Fatalf("%s did not register — this test would be measuring an unknown model, not an unpriced one", priceless)
	}

	// The provider serves three models: one the catalog prices, one it has never heard of, and the
	// priceless override. The middle one is the positive control — if it stops being reported, this
	// test is measuring a broken watcher rather than the rule.
	srv := modelsServer(t, "gpt-4o", "gpt-never-heard-of-4m8k", priceless)
	defer srv.Close()

	findings, errs := watcherAgainst(srv.URL, nil).Check(context.Background())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	reported := map[string]bool{}
	for _, f := range findings {
		reported[f.ModelID] = true
	}
	if reported["gpt-4o"] {
		t.Errorf("CONTROL: gpt-4o is fully priced and must never be reported — this detector is now noisy")
	}
	if !reported["gpt-never-heard-of-4m8k"] {
		t.Fatalf("CONTROL: an unknown model was NOT reported (findings: %+v) — the assertion below cannot fail", findings)
	}
	if !reported[priceless] {
		t.Errorf("a model registered with NO PRICE was not reported as drift (findings: %+v). "+
			"Lens cannot price it, bills it on a fallback, and the operator is never told the override "+
			"they added did not take", findings)
	}
}
