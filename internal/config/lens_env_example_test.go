package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// B18.11 — an operator can discover every setting. Every variable config.go reads is documented, with its
// default and one line of meaning: in lens.env.example, or — for the curated set docker-compose.yaml names
// under the lens service's `environment:` — in .env.example, because a curated variable set in lens.env
// arrives EMPTY (see lens_env_shadow_test.go).
func TestEveryVariableConfigReadsIsDocumented(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	names := func(re, src string) map[string]bool {
		set := map[string]bool{}
		for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(src, -1) {
			set[m[1]] = true
		}
		return set
	}

	reads := names(`(?:Getenv|LookupEnv|getEnv|parseBoolEnv|parseBoolEnvDefaultTrue)\("([A-Z][A-Z0-9_]+)"`, read("internal/config/config.go"))
	if len(reads) < 150 || !reads["LENS_DATABASE_URL"] || !reads["LENS_KEEL_ENABLED"] {
		t.Fatalf("found %d variables read by config.go — the parser is broken and this test would pass vacuously", len(reads))
	}
	curated := names(`(?m)^\s*-\s*([A-Z][A-Z0-9_]*)(?:=|$)`, lensServiceEnvBlock(read("docker-compose.yaml")))
	lensEnv := names(`(?m)^#?\s*([A-Z][A-Z0-9_]+)=`, read("lens.env.example"))
	dotEnv := names(`(?m)^#?\s*([A-Z][A-Z0-9_]+)=`, read(".env.example"))

	for v := range reads {
		switch {
		case curated[v] && !dotEnv[v]:
			t.Errorf("%s is read by config.go and set through docker-compose.yaml's environment:, but .env.example does not document it", v)
		case !curated[v] && !lensEnv[v]:
			t.Errorf("%s is read by config.go but lens.env.example does not document it: add `# %s=<default>   # <what it does>`", v, v)
		}
	}
}
