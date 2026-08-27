package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// env_census_test.go — every LENS_* name the binary READS against every one a
// shipped artefact DOCUMENTS, and every name config.go's own field comments CLAIM.
//
// ⚠ THE CLASS IS THIS QUEUE'S OWN: "a chart documenting a setting that never
// existed", and W4.22 (talyvor-code #59) found its mirror next door — live feature
// gates read from env vars that appear in no flag, no usage text and no document.
// Both directions are asserted here, at ZERO, because at the time of writing both
// are zero: 167 env-read names, all documented; 26 field comments naming a LENS_*
// var, all of them read.
//
// ⚠ WHY IT IS ZERO AND NOT A RATCHET. A ratchet ("the residue may shrink, never
// grow") is what you write when the residue is too large to clear. It was not: the
// count went 57 → 49 → 7 → 3 → 0 as the census's own population was corrected, and
// the last three were closed by writing them down. Asserting zero is a much stronger
// claim and it is the true one.
//
// ⚠⚠ THE MEASUREMENT LIED THREE TIMES BEFORE IT WAS RIGHT, AND THE EXCLUSIONS BELOW
// ARE WHAT THAT COST. Each is a rule, not a curated list:
//
//   - `.env.example` WAS NOT IN THE POPULATION. The first two counts (57, then 49)
//     were wrong by SEVEN TIMES because the sweep read `lens.env.example` — 14 names
//     — and not the git-tracked `.env.example`, which documents 168. A census is its
//     population boundary; this one had a whole file outside it.
//   - WILDCARD FAMILY FORMS. `LENS_TRACK_WEBHOOK_*` in a compose file truncates to
//     `LENS_TRACK_WEBHOOK_` under a `[A-Z0-9_]+` scan, and that prefix documents its
//     members. Treated as a family rather than as a phantom name.
//   - THE CENSUS WAS DOCUMENTED BY ITS OWN CONTROLS. `scripts/` holds 25 Python
//     measurement harnesses and 2 shell operator tools, and the harnesses carry
//     LENS_* names as MUTATION ANCHORS. Control P1 planted a read of
//     LENS_ZZZ_UNDOCUMENTED_SETTING and the census reported it as DOCUMENTED —
//     by the control script that planted it. A measurement whose population
//     contains its own instrument cannot fail. `scripts/*.py` is excluded for
//     that reason; `scripts/*.sh` stays, because onboard-trial-user.sh and
//     verify-staging-economy.sh are things an operator actually runs.
//   - A NAME IN A COMMENT IS NOT A READ. `LENS_EVENTS` is a NATS stream name;
//     `LENS_NODE_TLS_CA` appears only in a comment about future support. The read
//     side counts an env CALL — Getenv / LookupEnv / parse*Env — never a mention.

var (
	envName  = regexp.MustCompile(`\bLENS_[A-Z0-9_]+`)
	envRead  = regexp.MustCompile(`(?:Getenv|LookupEnv|parse[A-Za-z0-9_]*Env[A-Za-z0-9_]*)\(\s*"(LENS_[A-Z0-9_]+)"`)
	fieldDoc = regexp.MustCompile(`^\s*([A-Z][A-Za-z0-9_]*)\s+\S+\s+//.*?(LENS_[A-Z0-9_]+)`)
)

// documentationArtefacts — what an operator can actually read. `.env.example` is
// first because leaving it out is the mistake this census already made.
var documentationArtefacts = []string{
	".env.example", "lens.env.example", "README.md",
	"docker-compose.yaml", "docker-compose.dev.yaml", "docker-compose.trial.yaml",
	"docker-compose.trial-distill.yaml", "docker-compose.override.yml",
	"deploy", "docs", "scripts",
	"ADDING-A-PROVIDER.md", "ROADMAP.md", "BUILD_STATE.md", "COORDINATION.md",
}

const (
	envReadFloor = 150 // 167 env-read names when written
	envDocFloor  = 150 // 184 documented names when written
)

func envRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod — the sweep would cover nothing", root)
	}
	return root
}

// envNamesRead returns names reached by an actual env CALL, never a mention.
func envNamesRead(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "rel":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range envRead.FindAllStringSubmatch(string(raw), -1) {
			if _, seen := out[m[1]]; !seen {
				out[m[1]] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read sweep: %v", err)
	}
	return out
}

// envNamesDocumented returns every LENS_* name any shipped artefact mentions, plus
// the wildcard family prefixes.
func envNamesDocumented(t *testing.T, root string) (names map[string]string, families []string) {
	t.Helper()
	names = map[string]string{}
	for _, a := range documentationArtefacts {
		full := filepath.Join(root, a)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		var files []string
		if info.IsDir() {
			_ = filepath.Walk(full, func(p string, fi os.FileInfo, e error) error {
				if e != nil || fi.IsDir() {
					return nil
				}
				// ⚠ A MEASUREMENT HARNESS IS NOT DOCUMENTATION. The control campaigns
				// under scripts/ carry LENS_* names as mutation anchors, and counting
				// them makes this census unable to fail — see the note at the top.
				if strings.HasSuffix(p, ".py") {
					return nil
				}
				files = append(files, p)
				return nil
			})
		} else {
			files = []string{full}
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			rel, _ := filepath.Rel(root, f)
			for _, n := range envName.FindAllString(string(raw), -1) {
				if strings.HasSuffix(n, "_") {
					families = append(families, n)
					continue
				}
				if _, seen := names[n]; !seen {
					names[n] = rel
				}
			}
		}
	}
	sort.Strings(families)
	return names, families
}

func isDocumented(n string, doc map[string]string, families []string) bool {
	if _, ok := doc[n]; ok {
		return true
	}
	for _, f := range families {
		if strings.HasPrefix(n, f) {
			return true
		}
	}
	return strings.HasPrefix(n, "LENS_TEST_") // test-harness only; not an operator setting
}

func TestEveryEnvVarTheBinaryReadsIsDocumented(t *testing.T) {
	root := envRepoRoot(t)
	read := envNamesRead(t, root)
	doc, families := envNamesDocumented(t, root)

	if len(read) < envReadFloor {
		t.Fatalf("the read sweep found %d LENS_* env calls, want >= %d — it has broken, and a "+
			"broken sweep reports nothing undocumented", len(read), envReadFloor)
	}
	if len(doc) < envDocFloor {
		t.Fatalf("the documentation sweep found %d LENS_* names, want >= %d — check that "+
			"`.env.example` is still in documentationArtefacts. Leaving it out is the mistake "+
			"this census already made, and it made the answer wrong by seven times.",
			len(doc), envDocFloor)
	}

	var missing []string
	for n, where := range read {
		if !isDocumented(n, doc, families) {
			missing = append(missing, n+"  (read in "+where+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d LENS_* setting(s) the binary reads appear in NO shipped artefact — not "+
			".env.example, not a compose file, not deploy/, not docs/:\n  %s\n"+
			"    An operator cannot set what they cannot find. If one is deliberately "+
			"undocumented, write that down where somebody will read it.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// ⚠ THE MIRROR, AND THE ONE THAT FOUND SOMETHING. config.go's struct-field comments
// are the authoritative place an engineer looks for "what env var sets this". Two of
// them named env vars that nothing read: KeelInterval and KeelLookback are assigned
// from constants, and their comments read exactly like the env-loaded fields around
// them. An operator setting LENS_KEEL_INTERVAL=10m got nothing, silently.
func TestEveryEnvVarConfigGoClaimsIsActuallyRead(t *testing.T) {
	root := envRepoRoot(t)
	read := envNamesRead(t, root)

	raw, err := os.ReadFile(filepath.Join(root, "internal/config/config.go"))
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	claims := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		if m := fieldDoc.FindStringSubmatch(line); m != nil {
			claims[m[2]] = "config.go:" + itoaEnv(i+1) + " (" + m[1] + ")"
		}
	}
	if len(claims) < 15 {
		t.Fatalf("only %d config.go field comments name a LENS_* var, want >= 15 — the parse has "+
			"broken, and a broken parse finds no ghosts", len(claims))
	}

	var ghosts []string
	for n, where := range claims {
		if _, ok := read[n]; !ok {
			ghosts = append(ghosts, n+"  claimed at "+where)
		}
	}
	if len(ghosts) > 0 {
		sort.Strings(ghosts)
		t.Errorf("%d config.go field comment(s) name an env var that NOTHING reads:\n  %s\n"+
			"    config.go is where an engineer looks to answer \"what sets this\". A comment "+
			"naming a variable no code reads is a chart documenting a setting that never "+
			"existed — say CONSTANT, or add the read.", len(ghosts), strings.Join(ghosts, "\n  "))
	}
}

// ⚠ THE TEETH. Both tests above assert "the residue is empty", and a sweep that
// found nothing would satisfy both. This proves each side can classify.
func TestEnvCensusCanActuallyClassify(t *testing.T) {
	root := envRepoRoot(t)
	read := envNamesRead(t, root)
	doc, families := envNamesDocumented(t, root)

	for _, known := range []string{"LENS_ECONOMY_ENABLED", "LENS_BILLING_ENABLED"} {
		if _, ok := read[known]; !ok {
			t.Errorf("%s is not seen as read — the read sweep is blind, and a blind sweep "+
				"reports nothing undocumented", known)
		}
	}
	const invented = "LENS_ZZZ_NO_SUCH_SETTING_ZZZ"
	if isDocumented(invented, doc, families) {
		t.Errorf("%s is reported as documented — the documentation check accepts anything, so "+
			"\"every read name is documented\" is vacuous", invented)
	}
}

func itoaEnv(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
