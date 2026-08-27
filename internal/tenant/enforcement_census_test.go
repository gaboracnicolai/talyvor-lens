package tenant

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// enforcement_census_test.go — what actually reads a workspace's stored configuration.
//
// ⚠ NOTHING IS WIRED UP HERE AND THAT IS THE POINT OF THE ITEM (W6.27). Every field below is a
// threshold, a limit or a retention policy. Wiring one is choosing what it does to traffic that is
// flowing today, and this queue does not let a session take that call. What it can do is stop the
// repository describing the wiring as if it existed.
//
// THE SHAPE, measured against origin/main and not read off the comments:
//
//	PUT /v1/workspaces/{wsID}/config   accepts a tenant.WorkspaceConfig, validates it
//	                                   (`retention_days must be ≥ 0`) and persists it
//	GET /v1/workspaces/{wsID}/config   reads it back
//	components.schemas.WorkspaceConfig publishes every field
//	                                   → and tenant.WorkspaceConfig appears in NO other
//	                                     production file. Nothing enforces any of it.
//
// ⚠ THERE ARE TWO WORKSPACE-CONFIG SURFACES AND THE PUBLISHED ROUTE WRITES THE OTHER ONE. The
// allowlists that DO gate traffic are workspace.Workspace.AllowedModels/AllowedProviders, read by
// the proxy from the `workspaces` table. tenant.WorkspaceConfig is the `workspace_configs` table —
// the same table W6.26 found was missing from the erasure manifest, for the same underlying reason:
// two tables hold a workspace's configuration and it is easy to reason about the wrong one.
//
// So a caller who sets allowed_models through the documented admin route has NOT restricted
// anything: the proxy reads a different table. That is the sharpest consequence here and it needs
// a decision, not a guess.

// settableConfigFields are the fields PUT /v1/workspaces/{wsID}/config accepts and stores. Every
// one of them is a threshold, a limit, an allowlist or a retention policy, and not one is read by
// anything that gates traffic — measured, not asserted: TestTenantWorkspaceConfigHasNoEnforcementReader
// shows the TYPE has no production reader, so no field of it can have one either.
//
// The list is kept so it can be checked against the struct. A field added to WorkspaceConfig and
// not added here is a new published setting nobody has said anything about.
//
// ⚠ THIS LIST WAS WRONG ON ITS FIRST RUN AND THE CHECK BELOW IS WHY THAT IS KNOWN. It said
// "SpendingCap"; the field is SpendingCapUSD. A hand-written census of a struct's fields is a
// second source of truth, and this one drifted from the first before it was ever committed.
var settableConfigFields = []string{
	"SpendingCapUSD", "MonthlyBudget", "RateLimitRPM", "RateLimitTPM",
	"AllowedModels", "AllowedProviders", "LogLevel", "RetentionDays",
}

// ⚠ The list above must describe the struct, or the census is reasoning about a shape that has
// moved. This reads store.go's own declaration rather than reflecting over the type, because the
// json tags are what the published schema promises and they live in the source.
func TestSettableConfigFieldsMatchTheStruct(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "tenant", "store.go"))
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "type WorkspaceConfig struct {")
	if start < 0 {
		t.Fatal("WorkspaceConfig struct not found — the census is aimed at nothing")
	}
	end := strings.Index(body[start:], "\n}")
	if end < 0 {
		t.Fatal("WorkspaceConfig struct is unterminated")
	}
	decl := body[start : start+end]

	// Fields the route does not take as policy: identity, bookkeeping, and one projection.
	//
	// ⚠ APIKeys is READ-SIDE ONLY and it is worth saying why it is skipped rather than listed.
	// PUT decodes the whole struct, so a caller can send `api_keys` — and upsertConfigSQL has no
	// such column, so it is silently ignored on write. Skipped as not-a-setting; if that write
	// path ever gains the column it stops being a projection and belongs in the list.
	skip := map[string]bool{"ID": true, "Name": true, "CreatedAt": true, "UpdatedAt": true, "APIKeys": true}
	fieldRe := regexp.MustCompile(`(?m)^\t([A-Z]\w*)\s`)
	var got []string
	for _, m := range fieldRe.FindAllStringSubmatch(decl, -1) {
		if !skip[m[1]] {
			got = append(got, m[1])
		}
	}
	sort.Strings(got)
	want := append([]string(nil), settableConfigFields...)
	sort.Strings(want)
	if len(got) < 4 {
		t.Fatalf("parsed only %d settable fields from the struct (%v) — the parse is broken, and a "+
			"broken parse agrees with any list", len(got), got)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("WorkspaceConfig's settable fields have changed.\n  struct: %s\n  census: %s\n"+
			"A field ADDED is a new setting published through PUT /v1/workspaces/{wsID}/config — say "+
			"what enforces it, because as of W6.27 nothing enforces any of the others.",
			strings.Join(got, ", "), strings.Join(want, ", "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("root %s has no go.mod — the sweep would cover nothing", root)
	}
	return root
}

// productionGoFiles walks the tree, skipping tests, migrations and vendor.
func productionGoFiles(t *testing.T, skip func(rel string) bool) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "bin", "rel", "migrations", "scripts":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if skip != nil && skip(rel) {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("the walk found %d production .go files — it is broken, and a broken walk reports "+
			"a clean census", len(out))
	}
	return out
}

// ⚠ THE CENSUS. tenant.WorkspaceConfig must not gain a production reader without somebody saying
// what it now does — and equally, it must not LOSE one silently, because a reader appearing here
// is the good outcome this item is asking for.
func TestTenantWorkspaceConfigHasNoEnforcementReader(t *testing.T) {
	// The store defines the type; main.go decodes the PUT body into it. Those two are the surface,
	// not enforcement, and are excluded by name so the exclusion is visible.
	files := productionGoFiles(t, func(rel string) bool {
		return rel == filepath.Join("internal", "tenant", "store.go")
	})
	typeUse := regexp.MustCompile(`\btenant\.WorkspaceConfig\b`)

	var readers []string
	for rel, src := range files {
		for _, line := range strings.Split(src, "\n") {
			if !typeUse.MatchString(line) {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // prose is checked by TestNoCommentClaimsTenantConfigIsEnforced
			}
			readers = append(readers, rel+": "+strings.TrimSpace(line))
		}
	}
	sort.Strings(readers)

	const wantOnly = "cmd/lens/main.go: var in tenant.WorkspaceConfig"
	if len(readers) != 1 || readers[0] != wantOnly {
		t.Errorf("tenant.WorkspaceConfig is used in %d production place(s):\n  %s\n\n"+
			"W6.27 measured exactly one — the PUT handler decoding the request body. Anything else "+
			"means a field that was inert now DOES something (say what, and to whose traffic) or "+
			"that the decode moved (then this census is aimed at nothing).",
			len(readers), strings.Join(readers, "\n  "))
	}
}

// ⚠ CheckAllowed is the allowlist check this package exports. It has no production caller: the
// enforced allowlist is workspace.Workspace's, read from the `workspaces` table by the proxy.
func TestCheckAllowedHasNoProductionCaller(t *testing.T) {
	files := productionGoFiles(t, func(rel string) bool {
		return rel == filepath.Join("internal", "tenant", "store.go") // its own definition
	})
	// ⚠ NO TRAILING `\(`. The first draft required a call paren, and control U5 slipped
	// `handler := tenant.CheckAllowed` straight past it — a function VALUE is wiring just as
	// surely as a call is, and it is the shape a middleware would take. Matching the identifier
	// catches both.
	call := regexp.MustCompile(`\b(tenant\.)?CheckAllowed\b`)
	var callers []string
	for rel, src := range files {
		for _, line := range strings.Split(src, "\n") {
			if call.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				callers = append(callers, rel)
			}
		}
	}
	if len(callers) > 0 {
		sort.Strings(callers)
		t.Errorf("tenant.CheckAllowed now has %d production caller(s): %s.\n"+
			"    That is the outcome W6.27 asked for — but it means allowed_models set through "+
			"PUT /v1/workspaces/{wsID}/config now gates traffic that it did not gate before. Say so "+
			"deliberately, and check it against workspace.Workspace's allowlist, which already does.",
			len(callers), strings.Join(callers, ", "))
	}
}

// ⚠ RED FIRST — THE CLAIMS. Three comments describe this configuration as enforced. Each is quoted
// here as the substring that must NOT appear, because a comment is a falsifiable statement about
// the tree and these three are false.
//
// The third is the sharpest: main.go says per-workspace rate-limit tiers "are layered on at request
// time", and EIGHT LINES LATER blank-assigns the limiter with "exposed for future per-request
// wiring". Two claims, four lines apart, and only one of them is true.
func TestNoCommentClaimsTenantConfigIsEnforced(t *testing.T) {
	type claim struct{ file, phrase, why string }
	claims := []claim{
		{"cmd/lens/main.go",
			"per-workspace tiers are layered on at\n\t// request time by callers that build a per-request limiter\n\t// from tenant.WorkspaceConfig",
			"present tense, and no caller builds one — the same block blank-assigns the limiter " +
				"with \"exposed for future per-request wiring\""},
		{"internal/config/config.go",
			"the\n\t// per-workspace tier in MultiTierLimiter still applies",
			"MultiTierLimiter is constructed with exactly one tier, named \"global\". Setting " +
				"GlobalRPM=0 does not fall back to a per-workspace cap; it removes this limiter's cap"},
		{"internal/tenant/store.go",
			"The model allowlist check `CheckAllowed`\n// returns an error the proxy can surface to the client",
			"the proxy surfaces workspace.Workspace's allowlist, from the `workspaces` table; " +
				"CheckAllowed has no production caller at all"},
		{"internal/tenant/store.go",
			"DefaultRetentionDays is the policy when a workspace config",
			"it is not a policy — nothing deletes on it. Both time-based sweeps in the binary " +
				"(audit.Retention over token_events, and the semantic-cache sweep) take a single " +
				"global window from env and never read a per-workspace retention_days"},
	}
	root := repoRoot(t)
	for _, c := range claims {
		b, err := os.ReadFile(filepath.Join(root, c.file))
		if err != nil {
			t.Errorf("read %s: %v", c.file, err)
			continue
		}
		if strings.Contains(string(b), c.phrase) {
			t.Errorf("%s still claims tenant config is enforced:\n    %q\n  WHY IT IS FALSE: %s",
				c.file, strings.ReplaceAll(c.phrase, "\n", " "), c.why)
		}
	}
}
