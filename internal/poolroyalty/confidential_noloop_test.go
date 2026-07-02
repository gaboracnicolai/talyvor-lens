package poolroyalty

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// (proof 7) NO-LOOP + NO-LATENCY-DOUBLE-PAY: the confidential minter READS node_attestations ⋈
// inference_nodes ⋈ benchmark_node_scores by RAW SQL and WRITES only confidential_compute_mints + the
// ledger. It must import none of the attestation/capture/serve producers, and — critically — must reference
// NO latency signal (node_cohort_latency_stats / nodelatency), so it cannot double-pay what the latency mint
// already pays.
func TestConfidentialMinter_NoLoop_ImportGuard(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "confidential_minter.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenImports := []string{
		"internal/attestation", // the verify producer (writes node_attestations)
		"internal/nodelatency", // the latency capture — importing it risks a latency read (double-pay)
		"internal/benchprobe",  // the benchmark-score producer
		"internal/proxy",       // the serve path
	}
	for _, imp := range f.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		for _, bad := range forbiddenImports {
			if strings.Contains(p, bad) {
				t.Errorf("confidential_minter.go imports %q — it must not reach a signal producer (NO-LOOP)", p)
			}
		}
	}
	// NO latency dependency anywhere in the source (the anti-double-pay invariant).
	raw, err := os.ReadFile("confidential_minter.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	// The latency TABLE + column — if the minter's SQL referenced these it would double-pay latency. (The
	// nodelatency PACKAGE is already forbidden by the import guard above; not string-scanned, since the
	// doc-comment legitimately names it when explaining what is excluded.)
	for _, bad := range []string{"node_cohort_latency_stats", "latency_ewma"} {
		if strings.Contains(src, bad) {
			t.Errorf("confidential_minter.go references %q — it must NOT read latency (the latency mint owns that; no double-pay)", bad)
		}
	}
}
