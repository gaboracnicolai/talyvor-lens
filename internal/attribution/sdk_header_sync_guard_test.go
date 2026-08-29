package attribution

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE SYNC CLAIM BOTH SDKs MAKE, WITH A MECHANISM UNDER IT FOR THE FIRST TIME.
//
// sdk/python/talyvor_lens/types.py:  "The HEADER_* constants are the single source of truth for the
//
//	wire-level header names. Keep them in sync with the Go server-side handlers in internal/proxy
//	and internal/workspace — if the server adds a new X-Talyvor-* header, mirror it here."
//
// sdk/typescript/src/types.ts:       the same sentence.
//
// Neither had a mechanism. The SDKs are in no CI workflow, no Makefile target and no Go test, and
// the drift that resulted was X-Talyvor-Repository: emitted by both SDKs, documented in both
// READMEs, and stored by nothing.
//
// ⚠ WHAT THIS GUARD CANNOT DO, STATED SO IT IS NOT OVERQUOTED. A guard that only asks "does the
// name appear in a Header.Get somewhere" WOULD HAVE PASSED ON THE ORIGINAL DEFECT: X-Talyvor-Repository
// was read, by ExtractAttribution in this package's tracker.go, into Attribution.Repository — a field whose only non-test reader in
// the tree is nothing at all (proxy.go uses that struct solely as `attr.Branch != "" || attr.PRNumber
// != ""`). Name-presence is a NECESSARY condition, not a sufficient one. So each header is required
// to carry the NAME of the consumer that does something with it, and adding a header to an SDK
// cannot pass here until someone writes that name down. The behavioural half — that the value
// actually reaches storage — is sdk_header_contract_realpg_test.go, which is what caught this.
//
// If a header is genuinely fire-and-forget, say so in its reason. The guard's job is to make the
// absence of a consumer a thing somebody had to type, not a thing nobody noticed.
var sdkHeaderConsumers = map[string]string{
	"Authorization":        "internal/auth/middleware.go — bearer credential; the whole authz path",
	"X-Talyvor-Workspace":  "attribution ExtractFromRequest -> request_attribution.workspace_id; also ratelimit, workspace manager, batch submit",
	"X-Talyvor-Team":       "internal/proxy/proxy.go budget scoping (team); auth middleware also SETS it from the API key",
	"X-Talyvor-Feature":    "attribution ExtractFromRequest -> request_attribution.feature (with the code- prefix stripped); token_events.feature",
	"X-Talyvor-Session":    "attribution ExtractFromRequest -> request_attribution.session_id; proxy session pickup (sessionTracker.GetOrCreate)",
	"X-Talyvor-Agent":      "internal/proxy/proxy.go — agent name on the session record; defaults to \"default\"",
	"X-Talyvor-Branch":     "attribution ExtractFromRequest -> request_attribution.branch; echoed back on the response",
	"X-Talyvor-PR":         "attribution ExtractFromRequest -> request_attribution.pr_number; summaryByPRSQL",
	"X-Talyvor-Commit":     "attribution ExtractFromRequest -> request_attribution.commit_sha",
	"X-Talyvor-Repository": "attribution ExtractFromRequest -> request_attribution.repo_name; the predicate in branchSpendForWorkspaceSQL, topBranchesForWorkspaceSQL and summaryByRepoSQL",
}

var (
	pyHeaderRe = regexp.MustCompile(`(?m)^HEADER_[A-Z_]+\s*=\s*"([^"]+)"`)
	tsHeaderRe = regexp.MustCompile(`(?m)^export const HEADER_[A-Z_]+\s*=\s*"([^"]+)";`)
)

func headerConstants(t *testing.T, rel string, re *regexp.Regexp) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	m := re.FindAllStringSubmatch(string(b), -1)
	if len(m) == 0 {
		t.Fatalf("PARSER IS BLIND: no HEADER_* constants matched in %s. The file was found, so this is "+
			"the pattern rotting, not the SDK shrinking — a zero here must never read as agreement.", rel)
	}
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	sort.Strings(out)
	return out
}

// The two SDKs must declare the same wire names. A header one SDK sends and the other does not is a
// difference between two shipped clients that the server cannot see.
func TestBothSDKsDeclareTheSameHeaders(t *testing.T) {
	py := headerConstants(t, "sdk/python/talyvor_lens/types.py", pyHeaderRe)
	ts := headerConstants(t, "sdk/typescript/src/types.ts", tsHeaderRe)
	if strings.Join(py, "\n") != strings.Join(ts, "\n") {
		t.Errorf("the two SDKs declare different header sets.\npython (%d): %v\nts     (%d): %v",
			len(py), py, len(ts), ts)
	}
}

// Every header the SDKs declare must have a NAMED consumer, and every named consumer must still
// correspond to a header the SDKs declare. Both directions: a header added to an SDK with no
// consumer reds, and a consumer left behind after a header is dropped reds too.
func TestEverySDKHeaderHasANamedServerConsumer(t *testing.T) {
	py := headerConstants(t, "sdk/python/talyvor_lens/types.py", pyHeaderRe)

	declared := map[string]bool{}
	for _, h := range py {
		declared[h] = true
		reason, ok := sdkHeaderConsumers[h]
		if !ok {
			t.Errorf("the SDKs emit %q and no consumer is named for it in sdkHeaderConsumers.\n"+
				"Name the server code that does something with the value — or, if nothing does, say that,\n"+
				"because that is exactly the defect X-Talyvor-Repository was.", h)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("header %q has an empty consumer reason — an unexplained entry is not a classification", h)
		}
	}
	for h := range sdkHeaderConsumers {
		if !declared[h] {
			t.Errorf("sdkHeaderConsumers names a consumer for %q, which no SDK declares any more — "+
				"stale entry, delete it or restore the header", h)
		}
	}
}

// stripLineComments removes // comments so a MENTION cannot pass for a READ. Without this the guard
// is a census of spellings: a commented-out call, the header name inside a doc comment and a real
// Header.Get all match one substring, and the failure direction is a site that READS AS COVERED.
// Control C10 is exactly that mutation.
func stripLineComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// The header the SDKs emit for the repository must be the one ExtractFromRequest actually reads.
// This is the SOURCE-LEVEL mirror of the realpg behavioural test: it fails at compile-free grep
// speed in a repo with no database, so the defect cannot come back on a machine that skips the
// gated tests.
func TestExtractFromRequestReadsTheSDKRepositoryHeader(t *testing.T) {
	b, err := os.ReadFile("context.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	start := strings.Index(src, "func ExtractFromRequest(")
	if start < 0 {
		t.Fatal("ANCHOR LOST: ExtractFromRequest not found in context.go — this guard is measuring nothing")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("ANCHOR LOST: could not delimit ExtractFromRequest")
	}
	body := stripLineComments(src[start : start+end])
	if !strings.Contains(body, `"X-Talyvor-Repository"`) {
		t.Errorf("ExtractFromRequest does not read X-Talyvor-Repository — the only repository spelling any "+
			"shipped Talyvor client emits. request_attribution.repo_name is a WHERE predicate in three "+
			"queries; filling it from a header nothing sends makes all three inert.\nbody:\n%s", body)
	}
}
