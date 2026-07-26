package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ⚠ THIS GUARD WAS REWRITTEN WHEN `env_file:` LANDED, BECAUSE ITS OLD QUESTION BECAME UNANSWERABLE.
//
// It used to assert: "every variable named in a slog `remedy` field appears in the lens service's
// `environment:` list". That was the right check while `environment:` was the ONLY way in — a
// variable not listed there was mute, and the repo could prove it.
//
// docker-compose.yaml now forwards `lens.env` wholesale. Reachability therefore depends on a file
// that lives ON THE BOX and is not in this repository. The old assertion has two failure modes and
// both are bad: kept as-is it fails on variables that are perfectly reachable via lens.env, and
// relaxed to "environment: OR env_file exists" it can never fail again — a green no-op that reads
// like coverage. Several guards were retired today for exactly that, so this one is repointed at
// what is still decidable rather than left standing as decoration.
//
// WHAT IS STILL DECIDABLE FROM THE REPO, and what each check is worth:
//
//	1. THE MECHANISM IS INTACT. If someone deletes `env_file:` the whole class returns instantly and
//	   silently — 65 variables go mute again with no test failing. This is the load-bearing check.
//	2. IT FORWARDS lens.env AND NOT .env. `.env` carries TRACK_/DOCS_ gateway secrets on the deployed
//	   box (deploy/track-docs.compose.yaml runs in this project); forwarding it would hand two other
//	   services' secrets to Lens. A future "simplification" to `env_file: .env` must fail here.
//	3. OPERATORS ARE TOLD. Since the repo cannot see the box's lens.env, documentation is the only
//	   remaining lever: a variable an alert tells someone to set must appear in lens.env.example.
//
// What NO repo-side test can check any more: whether a given variable is actually set on a given
// box. That moved out of scope with env_file, and pretending otherwise would be the vacuous version.

// stripComments removes YAML comment lines. ⚠ LOAD-BEARING: the first version of this guard did a
// plain strings.Contains(blk, "env_file:") and PASSED with the directive deleted, because the
// explanatory comment above it also contains the word env_file. A guard that matches its own
// documentation about a mechanism, rather than the mechanism, cannot fail — caught by a positive
// control, which is the only reason it is not still green and wrong.
func stripComments(s string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func lensServiceBlock(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	s := string(raw)
	start := strings.Index(s, "\n  lens:\n")
	if start < 0 {
		t.Fatal("no `lens:` service in docker-compose.yaml — this guard cannot be evaluated, which " +
			"is a failure and not a pass")
	}
	rest := s[start+1:]
	// The next top-level service key ends the block.
	if end := regexp.MustCompile(`\n  [a-z][a-z0-9_-]*:\n`).FindStringIndex(rest[1:]); end != nil {
		return rest[:end[0]+1]
	}
	return rest
}

// 1 + 2: the forwarding mechanism exists and points at the right file.
func TestComposeForwardsLensEnvWholesale(t *testing.T) {
	blk := stripComments(lensServiceBlock(t))

	// The DIRECTIVE, at service-key indentation — not the word anywhere in the block.
	if !regexp.MustCompile(`(?m)^\s{4}env_file:\s*$`).MatchString(blk) {
		t.Fatal("the lens service has NO `env_file:`. Without it a variable reaches the process only " +
			"if someone remembered to add it to `environment:` — which left 65 of the 98 variables " +
			"internal/config reads MUTE (settable in .env, never delivered, indistinguishable from " +
			"unset from inside the process). Restore `env_file: - path: lens.env, required: false`.")
	}
	if !strings.Contains(blk, "lens.env") {
		t.Error("`env_file:` does not reference lens.env")
	}
	// ⚠ Must NOT forward the project .env: it carries TRACK_GATEWAY_AUTH_SECRET,
	// DOCS_GATEWAY_AUTH_SECRET and DOCS_WORKSPACE_ID on the deployed box.
	for _, bad := range []string{"- .env", "- path: .env", "env_file: .env"} {
		if strings.Contains(blk, bad) {
			t.Errorf("the lens service forwards the project .env (%q). That file holds Track's and "+
				"Docs's gateway auth secrets on the deployed box — Lens has no use for them and a Lens "+
				"compromise or crash dump would expose them. Forward lens.env instead.", bad)
		}
	}
}

// 3: a variable an alert tells an operator to set must be documented where they will look.
func TestEveryRemedyVariableIsDocumentedForOperators(t *testing.T) {
	root := filepath.Join("..", "..")

	example, err := os.ReadFile(filepath.Join(root, "lens.env.example"))
	if err != nil {
		t.Fatalf("read lens.env.example: %v — operators have nothing to copy", err)
	}
	doc := string(example)

	remedyVar := regexp.MustCompile(`"remedy"\s*,\s*((?:"[^"]*"\s*\+?\s*)+)`)
	envName := regexp.MustCompile(`LENS_[A-Z0-9_]+`)

	found := map[string]string{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "vendor" {
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

	// Vacuity control: this must actually be finding remedies.
	if len(found) == 0 {
		t.Fatal(`no "remedy" fields found in the tree — renamed, or this guard is passing vacuously`)
	}
	for v, file := range found {
		// Provider keys are set by the deployment's own secret handling, not this file.
		if v == "LENS_ANTHROPIC_API_KEY" || v == "LENS_OPENAI_API_KEY" || v == "LENS_PROVISION_SECRET" {
			continue
		}
		if !strings.Contains(doc, v) {
			t.Errorf("%s tells an operator to set %s, but lens.env.example never mentions it. The "+
				"repo can no longer verify a variable reaches the box, so documenting it is the only "+
				"lever left — an alert naming a variable an operator cannot find is an inoperable "+
				"instruction.", file, v)
		}
	}
}
