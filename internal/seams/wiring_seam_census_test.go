package seams

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// wiring_seam_census_test.go — exported Set*/Register*/Enable*/Attach* methods on
// internal/* types that NO PRODUCTION CODE EVER CALLS.
//
// ⚠ WHY A SEAM IS DIFFERENT FROM DEAD CODE. A `Set*` method is a socket somebody
// left for main.go to plug into. An unplugged socket is a feature that exists,
// compiles, is tested, and does nothing — and unlike dead code it READS AS FINISHED.
// W6.17 (#485) is the evidence: BatchRouter.SetSettleHook has no production caller,
// and that single fact is why main.go passes a hardcoded `false` for settleWired and
// the whole /v1/batch lane cannot open in any configuration.
//
// 112 seams found; 102 have a production caller. The TEN that do not are recorded
// below — and the split inside those ten is the point: SIX are unwired and SAY SO in
// their own doc comments (test seams, reserved anchors, test-only threshold
// overrides), and FOUR read as finished. Recording all ten rather than filtering the
// honest ones is what makes the difference visible.
//
// ⚠ THE CENSUS LIED TWICE BEFORE IT WAS RIGHT, and both lies pointed the same
// (flattering-to-me) way — calling a LIVE seam dead:
//
//   - SAME-PACKAGE CALLERS. The first pass looked only for callers OUTSIDE the
//     declaring package and flagged localrouter.Router.SetNodePrice and
//     benchprobe.Store.SetProbeScore. Both are called from their own package's
//     production code (multi.go's price-sync loop; scheduler.go). A seam used
//     internally is wired.
//   - THE FILE NAME IS EVIDENCE. proxy.SetAnthropicURL / SetGoogleURL live in
//     bench_setters.go and carry no doc comment, so a doc-comment keyword scan
//     called them live-looking. The file says what they are.

var seamDecl = regexp.MustCompile(`(?m)^func \([a-zA-Z_]\w*\s+\*?(\w+)\)\s+((?:Set|Register|Enable|Attach|Wire)[A-Z]\w*)\(`)

// unwiredSeams — declaring package → "Type.Method" → what its absence means.
// A seam here is called by TESTS ONLY.
var unwiredSeams = map[string]string{
	"internal/batch:BatchRouter.SetSettleHook": "⚠ RECORDED BY W6.17 (#485). No production caller, " +
		"and that is exactly why cmd/lens builds the lane gate as newBatchReg(cfg.BatchEnabled, " +
		"false) — a literal false for settleWired — so the /v1/batch routes are never registered " +
		"in ANY configuration. The socket being empty is load-bearing here, deliberately.",

	"internal/cache:SemanticCache.SetPooledWithVariants": "⚠ THE WRITER IS UNWIRED AND THE READER " +
		"IS NOT. SetPooled (no variants) IS called in production — internal/proxy/proxy.go and " +
		"internal/seedcache/seedcache.go — while SetPooledWithVariants, which writes the doc2query " +
		"match targets, is called only by tests. The read path is built for them: " +
		"semanticSelectPooledSQL selects COALESCE(variant_of, id). So doc2query variant rows are " +
		"created by nothing in production, and W2.7 records doc2query as DONE.",

	"internal/routingbrain:Store.SetAutonomous": "⚠ A DOCUMENTED PER-WORKSPACE OPT-IN WITH NO WAY " +
		"TO OPT IN. SetAutonomous is the only writer of routing_brain_autonomous, ModeFor reads " +
		"that table to return ModeAutonomous, and no route, handler or subcommand calls it — the " +
		"only callers are tests. Meanwhile cmd/lens LOGS \"advisory default, autonomous " +
		"per-workspace opt-in\" at boot and internal/config says the same. The only path to " +
		"autonomous routing is direct SQL. ⚠ THIS BOUNDS W6.20 (#488): the routing-brain cost " +
		"floor's consequences reach production only for a workspace in that table, and no product " +
		"surface can put one there.",

	"internal/economy:DualTokenStore.SetAgentCeiling": "⚠ A DOCUMENTED OPERATOR CAPABILITY WITH NO " +
		"SURFACE, ON A MONEY CAP. Its own doc says \"This is how an operator sets a per-agent cap " +
		"other than the default\" — and nothing calls it. No route, no handler, no subcommand. " +
		"Every scoped agent key therefore carries DefaultAgentCeilingLXC (50 LXC, $5) and no " +
		"operator can change it through the product.",

	// ── the remaining six are unwired AND SAY SO IN THEIR OWN DOC COMMENTS. They are
	// recorded rather than filtered out, because "unwired and honest about it" is a
	// different fact from "unwired and reads as finished", and a census that silently
	// drops the honest ones cannot show the difference.
	"internal/localrouter:Router.SetRand": "TEST SEAM, and says so: \"injects the selection RNG " +
		"(test-only; production uses math/rand/v2 top-level)\".",
	"internal/poolroyalty:Minter.SetAnchor": "RESERVED, and says so: \"reserved for the future " +
		"held-benchmark caller … Unused on any live path this PR\". main.go's own comment agrees.",
	"internal/poolroyalty:DistillMinter.SetAnchor": "RESERVED, same note as Minter.SetAnchor.",
	"internal/poolroyalty:EvalContributionMinter.SetMinConsensus": "TEST-ONLY OVERRIDE, and says " +
		"so: \"Lower is only for tests; production keeps DefaultMinConsensusAttesters\".",
	"internal/poolroyalty:EvalContributionMinter.SetMinUnlinkedGraders": "TEST-ONLY OVERRIDE, and " +
		"says so: \"production keeps DefaultMinUnlinkedGraders\".",
	"internal/poolroyalty:EvalContributionMinter.SetRequireConsensus": "TEST-ONLY TOGGLE, and says " +
		"so: \"Production keeps it ON (the default)\".",
}

const seamFloor = 90 // 115 seams found when written

func seamsRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod", root)
	}
	return root
}

type seam struct {
	pkgDir, typ, method string
}

func (s seam) key() string { return s.pkgDir + ":" + s.typ + "." + s.method }

// findSeams returns every exported wiring seam declared on an internal/* type.
func findSeams(t *testing.T, root string) []seam {
	t.Helper()
	var out []seam
	base := filepath.Join(root, "internal")
	err := filepath.Walk(base, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// ⚠ THE FILE NAME IS EVIDENCE. A *_setters.go / bench_* file is a test seam by
		// construction; treating it as production intent produced two false findings.
		if strings.Contains(filepath.Base(path), "bench_") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range seamDecl.FindAllStringSubmatch(string(raw), -1) {
			out = append(out, seam{filepath.Dir(rel), m[1], m[2]})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seam walk: %v", err)
	}
	return out
}

// productionCallers reports, for each seam, whether any NON-TEST file anywhere calls
// it — including its own package, which the first version of this census forgot.
func productionCallers(t *testing.T, root string, seams []seam) map[string]string {
	t.Helper()
	pats := make(map[string]*regexp.Regexp, len(seams))
	for _, s := range seams {
		if _, ok := pats[s.method]; !ok {
			pats[s.method] = regexp.MustCompile(`\.` + s.method + `\(`)
		}
	}
	hit := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
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
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		src := string(raw)
		for _, s := range seams {
			if _, done := hit[s.key()]; done {
				continue
			}
			// ⚠ NO "SKIP THE DECLARING FILE" RULE HERE, DELIBERATELY. A package may both
			// declare a seam and call it — localrouter does exactly that — and the regex
			// matches `.Method(`, which a `func (r *T) Method(` declaration does not.
			if pats[s.method].MatchString(src) {
				hit[s.key()] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("caller walk: %v", err)
	}
	return hit
}

func TestEveryWiringSeamHasAProductionCaller(t *testing.T) {
	root := seamsRepoRoot(t)
	seams := findSeams(t, root)
	if len(seams) < seamFloor {
		t.Fatalf("found %d wiring seams, want >= %d — the declaration parse has broken, and a "+
			"broken parse finds no unwired seams", len(seams), seamFloor)
	}
	callers := productionCallers(t, root, seams)
	if len(callers) < seamFloor-25 {
		t.Fatalf("only %d of %d seams have any caller — the caller sweep has broken, and a broken "+
			"sweep calls every live seam unwired", len(callers), len(seams))
	}

	var unwired []string
	for _, s := range seams {
		if _, ok := callers[s.key()]; !ok {
			unwired = append(unwired, s.key())
		}
	}
	sort.Strings(unwired)

	for _, u := range unwired {
		if _, ok := unwiredSeams[u]; !ok {
			t.Errorf("%s has no production caller — it is a socket nobody plugged in.\n"+
				"    A Set*/Register* method with no caller is a capability that exists, compiles, "+
				"is tested and does nothing, and it READS AS FINISHED. Say what its absence means "+
				"and record it, or wire it.", u)
		}
	}
	for want, why := range unwiredSeams {
		found := false
		for _, u := range unwired {
			if u == want {
				found = true
			}
		}
		if !found {
			t.Errorf("W6.25 records %s as having no production caller and it now has one. Good — "+
				"but the record is stale, and it says things about the system that are no longer "+
				"true:\n    %s", want, why)
		}
	}
	t.Logf("%d wiring seams, %d with a production caller, %d without: %v",
		len(seams), len(callers), len(unwired), unwired)
}

// ⚠ TEETH. The assertion above is "the unwired set is exactly this"; a caller sweep
// that matched everything would report an empty set and satisfy it.
func TestSeamCensusCanActuallyClassify(t *testing.T) {
	root := seamsRepoRoot(t)
	seams := findSeams(t, root)
	callers := productionCallers(t, root, seams)

	byName := map[string]bool{}
	for _, s := range seams {
		byName[s.method] = true
	}
	// A seam that is definitely wired must be seen as wired — SetHTTPClient is called
	// from cmd/lens for several miners (W6.11 measured those call sites).
	if !byName["SetHTTPClient"] {
		t.Error("SetHTTPClient is not seen as a declared seam — the declaration parse is blind")
	}
	wiredSeen := false
	for _, s := range seams {
		if s.method == "SetHTTPClient" {
			if _, ok := callers[s.key()]; ok {
				wiredSeen = true
			}
		}
	}
	if !wiredSeen {
		t.Error("no SetHTTPClient seam is seen as having a production caller — the caller sweep " +
			"is blind, and a blind sweep calls every seam unwired")
	}
	if _, ok := callers["internal/routingbrain:Store.SetAutonomous"]; ok {
		t.Error("SetAutonomous now has a production caller — either autonomous mode gained a " +
			"product surface (update the record) or the sweep counts a test as production")
	}
}
