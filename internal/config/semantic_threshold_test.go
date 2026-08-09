package config

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ⚠ WHY THIS FILE EXISTS. W2.2: Nicolai decided to raise the pooled-cache similarity threshold
// from 0.92 to 0.98. Re-measured through cmd/hitrate against the live embedder before the change
// (2026-08-09, text-embedding-3-small):
//
//	engineering  hit 2/30 at 0.98 AND at 0.88 — FLAT, the threshold is not the binding constraint
//	consumer     hit 0/38 at every value; production false-serve 2/42 at 0.92 -> 0/42 at 0.98
//
// The two consumer danger pairs the raise removes are notice-direction (0.9770) and isa-year
// (0.9377), both below 0.98 and both above 0.92.
//
// ⚠ AND THE MARGIN IS THINNER THAN THE DECISION NOTE SAYS. The note records the serving band as
// "nothing sits between 0.8488 and 0.9808". Measured, the upper edge is timeout-spacing at 0.9807
// — so one of the two surviving engineering hits clears 0.98 by 0.0007. It survives, but "zero
// cost" has seven ten-thousandths of headroom, not a comfortable gap. Pinned here so a future
// embedding-model change that moves that pair is read as the threshold question it is.
//
// ⚠ THE THRESHOLD IS THE SMALLER HALF OF W2.2 and this file does not pretend otherwise: the
// entity gate is inert on consumer traffic (an empty extraction compares equal to another empty
// extraction), which is a separate merge.

// TestDefaultSemanticThreshold_IsTheDecidedValue pins the constant to a HARDCODED literal.
//
// ⚠ IT DOES NOT COMPARE THE CONSTANT TO ITSELF. `want := DefaultSemanticThreshold` would pass for
// every value the constant could ever hold, which is a guard that cannot fail.
func TestDefaultSemanticThreshold_IsTheDecidedValue(t *testing.T) {
	const decided = 0.98 // Nicolai's W2.2 decision, re-measured before it was applied
	if DefaultSemanticThreshold != decided {
		t.Fatalf("DefaultSemanticThreshold = %v, want %v (W2.2's decided value)",
			DefaultSemanticThreshold, decided)
	}
}

// TestLoad_DefaultsToDecidedThreshold checks the value production actually boots with when the
// operator sets nothing — the literal in the Config struct, not the constant beside it.
func TestLoad_DefaultsToDecidedThreshold(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_SEMANTIC_THRESHOLD", "") // the override is skipped on empty, so this is "unset"

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.SemanticThreshold != DefaultSemanticThreshold {
		t.Fatalf("Load().SemanticThreshold = %v, want %v — the struct literal has drifted from "+
			"the exported constant, so production and every test bind to different values",
			cfg.SemanticThreshold, DefaultSemanticThreshold)
	}
}

// TestLoad_EnvStillOverrides guards the direction this merge must NOT break: an operator can
// still set the threshold. A default change that silently froze the knob would be a worse defect
// than the one it fixed.
func TestLoad_EnvStillOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_SEMANTIC_THRESHOLD", "0.95")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.SemanticThreshold != 0.95 {
		t.Fatalf("LENS_SEMANTIC_THRESHOLD=0.95 gave %v — the env override no longer reaches the field",
			cfg.SemanticThreshold)
	}
}

// TestEnvExampleAgreesWithTheDefault is the #96 guard: a value stated in two places with nothing
// between them drifts, and the copy nobody runs keeps asserting the old number in silence.
// `.env.example` is what an operator copies, so a stale value there ships the OLD threshold to
// every fresh deployment while every test here stays green.
func TestEnvExampleAgreesWithTheDefault(t *testing.T) {
	const path = "../../.env.example"
	b, err := os.ReadFile(path)
	if err != nil {
		// ⚠ A MISSING FILE IS RED, NOT SKIP. A file-reading guard that skips when the path breaks
		// is the usual way this kind of test goes quietly blind.
		t.Fatalf("cannot read %s: %v", path, err)
	}

	re := regexp.MustCompile(`(?m)^LENS_SEMANTIC_THRESHOLD=(.+)$`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s declares no LENS_SEMANTIC_THRESHOLD — it is the file operators copy, so the "+
			"threshold must be stated there", path)
	}
	got, err := strconv.ParseFloat(strings.TrimSpace(string(m[1])), 64)
	if err != nil {
		t.Fatalf("%s: LENS_SEMANTIC_THRESHOLD=%q does not parse: %v", path, m[1], err)
	}
	if got != DefaultSemanticThreshold {
		t.Fatalf("%s says LENS_SEMANTIC_THRESHOLD=%v but DefaultSemanticThreshold is %v — "+
			"two copies of one decision, and this is the copy operators deploy",
			path, got, DefaultSemanticThreshold)
	}
}

// TestConscheckBindsToTheSharedConstant closes the THIRD copy. cmd/conscheck hardcoded
//
//	th := 0.92 // config.DefaultSemanticThreshold — production
//
// a comment citing a symbol that did not exist, beside a literal nothing kept in sync. A
// consistency checker measuring a threshold production no longer runs reports on nothing.
func TestConscheckBindsToTheSharedConstant(t *testing.T) {
	const path = "../../cmd/conscheck/main.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	// ⚠ COMMENTS ARE STRIPPED BEFORE THE REFERENCE CHECK, and finding that out is why this line
	// exists. The defect being fixed was `th := 0.92 // config.DefaultSemanticThreshold — production`
	// — a bare literal beside a comment naming a symbol that did not exist. A raw Contains() over
	// the file is satisfied by that comment, so the guard would have accepted the exact code it was
	// written to reject. Caught by positive control C1, which went red by the OTHER branch.
	src := stripLineComments(string(b))

	if !strings.Contains(src, "config.DefaultSemanticThreshold") {
		t.Fatalf("%s does not reference config.DefaultSemanticThreshold — it holds its own copy of "+
			"the production threshold, which drifts the moment the real one moves", path)
	}
	// And no bare decimal may sit in the threshold assignment.
	if m := regexp.MustCompile(`th\s*:?=\s*0\.\d+`).FindString(src); m != "" {
		t.Fatalf("%s still assigns a literal threshold (%q); it must bind to the shared constant", path, m)
	}
}

// stripLineComments removes `//` comments so a guard reads CODE rather than prose about code.
// It is deliberately line-oriented and does not attempt to parse string literals containing "//"
// (a URL, say): over-stripping here can only make the reference check STRICTER, never blinder,
// because a real `config.DefaultSemanticThreshold` reference never sits inside a string.
func stripLineComments(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
