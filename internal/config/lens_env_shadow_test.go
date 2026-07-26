package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ⚠ A VARIABLE IN BOTH docker-compose.yaml's `environment:` AND lens.env.example IS SILENTLY BROKEN.
//
// Compose's `environment:` OVERRIDES `env_file`. The curated entries are written
// `- LENS_X=${LENS_X:-}`, which resolves from .env or the shell; if neither defines it, that resolves
// to EMPTY — and the empty wins over whatever lens.env says. So an operator who follows
// lens.env.example, sets the variable, restarts, and sees the feature still off has done everything
// right and gets no signal at all.
//
// ⚠ VERIFIED IN A REAL CONTAINER, not inferred from the docs. Both `environment:` spellings shadow:
//
//   - LENS_X=${LENS_X:-}   → arrives as ""      (shadows)
//   - LENS_X               → arrives as ""      (shadows; the bare form does NOT defer to env_file)
//     (absent from environment:) → arrives with lens.env's value
//
// So there is no spelling that lets a variable live in both places. They are mutually exclusive, and
// this test is what keeps them that way — the failure is invisible at runtime (an empty value is
// indistinguishable from "operator did not set it"), so it has to be caught here or not at all.
//
// This shipped broken once: seven variables were in both, including LENS_MODEL_CATALOG_OVERRIDES —
// the remedy the unpriced-model alert NAMES. Following that instruction exactly produced an empty
// value and no explanation. That is the second time that variable was made inoperable by a
// deployment mechanism rather than by its own code.
func TestNoVariableIsBothCuratedAndInLensEnvExample(t *testing.T) {
	root := filepath.Join("..", "..")

	compose, err := os.ReadFile(filepath.Join(root, "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	lensBlock := lensServiceEnvBlock(string(compose))
	if lensBlock == "" {
		t.Fatal("could not isolate the lens service block — this guard cannot be evaluated, which is a " +
			"failure rather than a pass")
	}
	curated := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*-\s*([A-Z][A-Z0-9_]*)(?:=|$)`).FindAllStringSubmatch(lensBlock, -1) {
		curated[m[1]] = true
	}
	// Positive control on the parser: a variable we know is curated must be found.
	if !curated["LENS_ANTHROPIC_API_KEY"] {
		t.Fatal("parser found no LENS_ANTHROPIC_API_KEY in the lens environment block — the instrument " +
			"is broken and every result below is meaningless")
	}

	example, err := os.ReadFile(filepath.Join(root, "lens.env.example"))
	if err != nil {
		t.Fatalf("read lens.env.example: %v", err)
	}
	documented := regexp.MustCompile(`(?m)^#?\s*(LENS_[A-Z0-9_]+)=`).FindAllStringSubmatch(string(example), -1)
	if len(documented) == 0 {
		t.Fatal("lens.env.example documents no variables — either it was emptied or this parser is " +
			"broken; both make the guard vacuous")
	}

	for _, m := range documented {
		v := m[1]
		if curated[v] {
			t.Errorf("%s is in BOTH docker-compose.yaml's environment: list and lens.env.example. "+
				"`environment:` overrides `env_file`, and the curated entry resolves from .env — so an "+
				"operator who sets this in lens.env gets an EMPTY value in the container and no signal. "+
				"Pick one: set it in .env (and keep it curated), or drop the curated entry so lens.env "+
				"can deliver it.", v)
		}
	}
}

// lensServiceEnvBlock returns the lens service's YAML block, or "" if it cannot be found.
func lensServiceEnvBlock(compose string) string {
	start := strings.Index(compose, "\n  lens:\n")
	if start < 0 {
		return ""
	}
	rest := compose[start+1:]
	if end := regexp.MustCompile(`\n  [a-z][a-z0-9_-]*:\n`).FindStringIndex(rest[1:]); end != nil {
		return rest[:end[0]+1]
	}
	return rest
}
