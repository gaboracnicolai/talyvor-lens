package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// compose_env_mechanism_test.go — guards the MECHANISM, not a list of members.
//
// ── WHY A SECOND GUARD ───────────────────────────────────────────────────────
//
// compose_env_reach_test.go asserts that a hand-maintained set of important
// variables is forwarded. That guard is real and it caught real bugs, but it can
// only ever be as complete as the person editing it: a variable is protected once
// somebody has decided it matters and typed it in. Four variables were found mute
// by accident in a single day (LENS_PROVISION_SECRET, two webhook URLs,
// LENS_CACHE_POOLABLE_ENABLED) — each one discovered by an unrelated
// investigation, not by a gate. Adding the fifth to a list does not stop the
// sixth.
//
// This test takes the other half of the problem: rather than checking members, it
// checks that the forwarding MECHANISM cannot be silent by omission at all. If the
// lens service passes its environment through wholesale (env_file), then a newly
// added config variable is reachable the moment an operator sets it, and no list
// has to be edited for it — so there is no drift to guard against. If the service
// ever goes back to enumerating variables one by one, this fails and names every
// variable that would become mute.
//
// Both sides of that are DERIVED: the config-read set is parsed out of
// internal/config, and the reachable set is parsed out of the compose file. No
// human keeps either in sync.
//
// ── WHAT REPLACES THE LIST'S DOCUMENTATION ───────────────────────────────────
//
// The explicit list had one genuine virtue: reading it told you what Lens
// consumes. env_file loses that, so it is replaced by something better —
// .env.example, which is checked against the config-read set by
// TestEnvExampleDocumentsEveryConfigVariable below. A generated-and-verified
// document beats a hand-kept list that is also load-bearing for whether the
// software works.
//
// ── ON LEAKAGE ───────────────────────────────────────────────────────────────
//
// env_file forwards the whole file, so anything unrelated sitting in .env enters
// the container. That was decided deliberately, not waved through: on the
// documented shape (.env.example, .env.production.example) the only non-LENS_
// variable is POSTGRES_PASSWORD, and the lens container ALREADY receives it —
// it is interpolated into LENS_DATABASE_URL. So the mechanism exposes nothing the
// process did not already hold. TestEnvFileLeakSurfaceIsBounded pins that, and
// fails if a future non-LENS_ variable appears in the documented env files, so the
// decision is re-made deliberately rather than inherited.

var (
	// envNameRE finds a LENS_* variable name inside a Go string literal.
	envNameRE = regexp.MustCompile(`"(LENS_[A-Z0-9_]+)"`)
	// listEntryRE matches a real compose environment list entry (NOT a comment
	// mentioning the name — the first version of the sibling guard was defeated by
	// exactly that, and the lesson is kept here).
	listEntryRE = regexp.MustCompile(`(?m)^\s*-\s*(LENS_[A-Z0-9_]+)=`)
	// envFileRE matches an env_file key that is a real YAML key, not prose.
	envFileRE = regexp.MustCompile(`(?m)^\s+env_file:`)
	// serviceHeadRE matches a top-level service key (two-space indent). Go's RE2 has
	// no lookahead, so the block is cut by scanning lines rather than by regexp.
	serviceHeadRE = regexp.MustCompile(`^  ([a-z0-9-]+):\s*$`)
	// exampleEntryRE matches a documented variable in a .env example file,
	// including commented-out ones (`# LENS_FOO=`), which still document.
	exampleEntryRE = regexp.MustCompile(`(?m)^#?\s*([A-Z][A-Z0-9_]*)=`)
)

func repoRootT(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// configReadVars derives every LENS_* variable internal/config reads. Source of
// truth is the code, so a variable added there is covered without anyone
// remembering this test exists.
func configReadVars(t *testing.T, root string) map[string]bool {
	t.Helper()
	dir := filepath.Join(root, "internal", "config")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/config: %v", err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range envNameRE.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
	}
	if len(out) == 0 {
		t.Fatal("derived zero config variables — the parser is broken, not the repo")
	}
	return out
}

// lensServiceBlock returns the compose text for the lens service.
func lensServiceBlock(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "docker-compose.yaml"))
	if err != nil {
		t.Fatalf("read docker-compose.yaml: %v", err)
	}
	var out []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if m := serviceHeadRE.FindStringSubmatch(line); m != nil {
			if in {
				break // next service starts: the lens block is complete
			}
			in = m[1] == "lens"
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		t.Fatal("lens service not found in docker-compose.yaml")
	}
	return strings.Join(out, "\n")
}

// TestComposeForwardsEnvironmentWholesale is the mechanism guard. A pass-through
// mechanism means a config variable cannot be mute by omission — which is the only
// way to stop the drift, because the drift is caused by needing to remember.
func TestComposeForwardsEnvironmentWholesale(t *testing.T) {
	root := repoRootT(t)
	blk := lensServiceBlock(t, root)

	if envFileRE.MatchString(blk) {
		return // wholesale pass-through: nothing can be mute by omission.
	}

	// No pass-through ⇒ a variable reaches the process only if it is enumerated.
	// Name every one that would be mute, so the failure is actionable rather than
	// a verdict.
	listed := map[string]bool{}
	for _, m := range listEntryRE.FindAllStringSubmatch(blk, -1) {
		listed[m[1]] = true
	}
	var mute []string
	for v := range configReadVars(t, root) {
		if !listed[v] {
			mute = append(mute, v)
		}
	}
	sort.Strings(mute)

	preview := mute
	if len(preview) > 15 {
		preview = preview[:15]
	}
	t.Errorf("the lens service enumerates its environment instead of passing it through, so %d of the "+
		"variables internal/config reads CANNOT reach the process however they are set:\n\t%s\n\t… (%d total)\n\n"+
		"Add `env_file: .env` to the lens service. A list has to be edited for every new variable, and "+
		"four were found mute by accident in one day — the mechanism is the bug, not the missing entries.",
		len(mute), strings.Join(preview, "\n\t"), len(mute))
}

// TestEnvExampleDocumentsEveryConfigVariable replaces the documentation the
// explicit compose list used to provide, and does it better: the compose list
// documented only what somebody had added, whereas this fails when config grows a
// variable the operator-facing example does not mention.
func TestEnvExampleDocumentsEveryConfigVariable(t *testing.T) {
	root := repoRootT(t)
	documented := map[string]bool{}
	for _, name := range []string{".env.example", ".env.production.example"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue // an absent example file is covered by the emptiness check below
		}
		for _, m := range exampleEntryRE.FindAllStringSubmatch(string(b), -1) {
			documented[m[1]] = true
		}
	}
	if len(documented) == 0 {
		t.Fatal("no .env example file documents anything — the parser or the files are broken")
	}

	var undocumented []string
	for v := range configReadVars(t, root) {
		if !documented[v] {
			undocumented = append(undocumented, v)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		preview := undocumented
		if len(preview) > 20 {
			preview = preview[:20]
		}
		t.Errorf("%d config variables are undocumented in the .env example files — an operator has no way "+
			"to discover them:\n\t%s\n\t… (%d total)",
			len(undocumented), strings.Join(preview, "\n\t"), len(undocumented))
	}
}

// TestEnvFileLeakSurfaceIsBounded pins the leak decision. env_file forwards the
// whole file, so this fails if a non-LENS_ variable other than the already-known
// POSTGRES_PASSWORD appears in a documented env file — forcing the next person to
// decide deliberately whether that value belongs inside the lens container.
func TestEnvFileLeakSurfaceIsBounded(t *testing.T) {
	root := repoRootT(t)
	// POSTGRES_PASSWORD is already inside the container: docker-compose.yaml
	// interpolates it into LENS_DATABASE_URL. Forwarding it by name discloses
	// nothing new.
	allowed := map[string]bool{"POSTGRES_PASSWORD": true}

	var unexpected []string
	for _, name := range []string{".env.example", ".env.production.example"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		for _, m := range exampleEntryRE.FindAllStringSubmatch(string(b), -1) {
			v := m[1]
			if !strings.HasPrefix(v, "LENS_") && !allowed[v] {
				unexpected = append(unexpected, name+":"+v)
			}
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("non-LENS_ variables appear in the documented env files and would now enter the lens "+
			"container via env_file:\n\t%s\n\n"+
			"Decide deliberately: either the value belongs in the container (add it to the allowlist here "+
			"with the reason), or it belongs in a file the lens service does not read.",
			strings.Join(unexpected, "\n\t"))
	}
}
