package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// IF WE TELL AN OPERATOR TO SET A VARIABLE, THAT VARIABLE MUST BE ABLE TO REACH THE CONTAINER.
//
// docker-compose.yaml gives the lens service an explicit `environment:` list and NO `env_file:`.
// A variable not named in that list is unreachable: putting it in .env does nothing, the container
// starts healthy, and the feature is silently inert. Two live instances of exactly that:
//
//   - LENS_PROVISION_SECRET — every login 404'd, nothing in the logs said why (fixed in #368).
//   - LENS_MODEL_CATALOG_OVERRIDES — the remedy the unpriced-model alert NAMES ("add the model via
//     LENS_MODEL_CATALOG_OVERRIDES, no rebuild"). An operator following that instruction exactly
//     would have seen nothing change, with no way to tell why.
//
// The second one is worse than a missing feature: it is an alert that gives an inoperable
// instruction, which spends the operator's trust in every future alert.
//
// ⚠ THE RULE IS DERIVED, NOT CURATED, because a hand-maintained list is only as wide as the list and
// reads exactly like a passing one. The signal is structural: a slog field literally keyed "remedy".
// Write a new remedy naming a new variable and this test starts covering it with no edit here.
//
// It deliberately does NOT cover every LENS_* mentioned anywhere in a string — that is ~77 vars,
// almost all config.go validation messages ("invalid LENS_X (Go duration)"), which are not
// instructions to go set something. A gate that red-walls on 77 findings gets suppressed, and a
// suppressed gate is worse than none.
func TestComposePassesEveryVariableAnAlertTellsOperatorsToSet(t *testing.T) {
	root := filepath.Join("..", "..")

	composeRaw, err := os.ReadFile(filepath.Join(root, "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	declared := composeDeclaredEnv(string(composeRaw))
	if len(declared) == 0 {
		t.Fatal("parsed NO env vars out of docker-compose.yaml — the parser is broken, so this " +
			"guard would pass vacuously for the opposite reason it exists")
	}
	// Positive control on the parser itself: a var we know is wired must be found.
	if !declared["LENS_ANTHROPIC_API_KEY"] {
		t.Fatal("parser did not find LENS_ANTHROPIC_API_KEY, which IS in the compose file — the " +
			"instrument is broken and every result below is meaningless")
	}

	remedyVar := regexp.MustCompile(`"remedy"\s*,\s*((?:"[^"]*"\s*\+?\s*)+)`)
	envName := regexp.MustCompile(`LENS_[A-Z0-9_]+`)

	found := map[string]string{} // var -> file that names it
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == ".git" || info.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range remedyVar.FindAllStringSubmatch(string(src), -1) {
			for _, v := range envName.FindAllString(m[1], -1) {
				if _, seen := found[v]; !seen {
					found[v] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal(`no "remedy" fields found in the tree — either they were renamed or this guard is ` +
			`passing vacuously. It must cover at least the modelwatch and provisioning remedies.`)
	}

	for v, file := range found {
		if !declared[v] {
			t.Errorf("%s tells an operator to set %s, but docker-compose.yaml never passes it to the "+
				"container — setting it in .env would do NOTHING and the operator gets no signal. "+
				"Add `- %s=${%s:-}` to the lens service's environment list.", file, v, v, v)
		}
	}
}

// composeDeclaredEnv reads BOTH compose env syntaxes. The lens service uses the list form
// (`- VAR=${VAR}`) while migrate uses the map form (`VAR: value`) — a parser that handles only one
// returns a confident, wrong answer. (It reported LENS_ANTHROPIC_API_KEY missing when it is plainly
// present, which is how this was caught.)
func composeDeclaredEnv(s string) map[string]bool {
	out := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*-\s*([A-Z][A-Z0-9_]*)=`),
		regexp.MustCompile(`(?m)^\s{6,}([A-Z][A-Z0-9_]*)\s*:`),
	} {
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			out[m[1]] = true
		}
	}
	return out
}
