package alerts

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
)

// unpriced_zero_caller_census_test.go — every caller of the PLAIN alerts.CostUSD,
// and what reads the zero it returns for a model the catalog does not price.
//
// ⚠ THE FACT THIS RESTS ON. CostUSD → costUSD → catalog.Price, which returns
// ok=false for an unknown id, and costUSD then returns EXACTLY 0. The catalog holds
// 45 models; `gpt-4`, `gpt-4-turbo` and `claude-3-opus` are not among them. So every
// caller below books, displays or DECIDES on a zero for those models. The sibling
// CostUSDResolved falls back to a derived rate and announces it through
// WarnUnpricedModel; the plain helper cannot.
//
// ⚠ WHY THIS IS A CENSUS AND NOT A SWEEP OF FIXES. Two callers have been repaired,
// and each was mergeable for the same specific reason: it had a TWIN on the same
// path that had already decided the direction.
//
//	W6.16 (#484)  the streamed settle   ← the buffered settle already used the
//	                                      resolver, and says so in its own comment
//	W6.19 (#487)  the budget gate       ← the LXC gate beside it already used the
//	                                      resolver with catalog.PurposeCharge
//
// NONE OF THE NINE BELOW HAS SUCH A TWIN. For each of them, choosing a fallback is
// choosing what an unpriced model costs on that surface — and the two directions are
// not close: on a 1000/1000-token request PurposeCharge and PurposeHold differ by
// orders of magnitude (asserted below, so this is not a claim). That is a price
// decision, and W6.20 records it rather than taking it.
//
// The census fails when the caller set CHANGES — a new caller inherits the zero
// silently, and a removed one means somebody decided something that belongs in the
// record.

var costUSDCall = regexp.MustCompile(`\balerts\.CostUSD\(`)

// remainingCallers — file → the number of CALL SITES in it, and what reads the zero.
//
// ⚠ THE COUNT IS PART OF THE RECORD, AND CONTROL M1 IS WHY. The first draft compared
// only the SET of files, so a SECOND call appearing in an already-listed file was
// invisible — the census claimed to catch "a new caller silently inherits the zero"
// and only caught it when the caller landed in a new file. Counting sites closes it.
type callerRecord struct {
	sites int
	reads string
}

var remainingCallers = map[string]callerRecord{
	"cmd/lens/main.go": {2, "TWO: the routing advisor's blended price (line ~1528) and the routing " +
		"BRAIN's hard-floor cost basis (~1640). ⚠ THE BRAIN ONE DECIDES ROUTING, and the three " +
		"consequences are driven in internal/routingbrain/unpriced_cost_floor_test.go: an unpriced " +
		"recommendation always clears the cost cap, an unpriced candidate wins every quality tie, " +
		"and an unpriced SAFE model refuses every priced recommendation. The same zero waves " +
		"through in one direction and blocks in the other, which is exactly why 'just add a " +
		"fallback' is not obviously right here."},
	"internal/batch/router.go": {1, "job.CostUSD, multiplied by BatchDiscount and handed to the settle " +
		"hook — a money path, but one that cannot fire: SetSettleHook has no production caller and " +
		"the lane is closed in every configuration (W6.17/#485)."},
	"internal/eval/cost.go":     {1, "EstCostUSD, the eval run's pre-flight estimate shown to the caller."},
	"internal/eval/pipeline.go": {1, "result.CostUSD → RunSummary.TotalCostUSD, the figure an eval run reports (W6.14/#482)."},
	"internal/mcp/server.go": {1, "estimated_cost_per_1k_tokens, returned to an MCP caller as advice. A " +
		"zero tells a user an unpriced model is free."},
	"internal/proxy/distill_integration.go": {1, "avoidedCOGSUSD on a cross-tenant pooled OCR serve — it " +
		"feeds a ROYALTY MARGIN, so a zero UNDER-PAYS a contributor. ⚠ Direction is a payout " +
		"design question and this queue holds payout design open (W6.3.2, W6.3.3)."},
	"internal/proxy/proxy.go":            {1, "turnCost on the session-turn record — the per-turn figure a session summary reports."},
	"internal/proxy/worktier_capture.go": {1, "the cost signal into worktier.Classify and the worktier sink. Descriptive by design (the admin route table calls worktier 'money-decoupled; never feeds mint/earn/billing'), so a zero mis-TIERS rather than mis-bills."},
}

// commentMentions — files that name alerts.CostUSD in prose only. Kept so the sweep
// can prove it excluded them deliberately rather than by accident.
// ⚠ THIS LIST WAS THREE ENTRIES LONG FOR ABOUT A MINUTE. W6.19 moved the budget gate
// onto budgetEstimateUSD, which left internal/proxy/lxc_gate.go and
// internal/workspace/compression_policy.go describing a call that no longer existed;
// W6.20 corrected both comments in the same tree, and this census then (correctly)
// fired on a list that still expected them to mention alerts.CostUSD. The guard
// caught its own author's stale exclusion within one run of being written, which is
// the behaviour a comment census is for.
var commentMentions = []string{
	"internal/proxy/budget_gate_estimate.go",
}

func repoRootForCensus(t *testing.T) string {
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

// sweepCostUSDCallers returns file → call-site count, counting only lines that are
// not comments.
func sweepCostUSDCallers(t *testing.T) (calls map[string]int, comments map[string]int) {
	t.Helper()
	root := repoRootForCensus(t)
	calls, comments = map[string]int{}, map[string]int{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
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
		for _, line := range strings.Split(string(raw), "\n") {
			if !costUSDCall.MatchString(line) {
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				comments[rel]++
				continue
			}
			calls[rel] += len(costUSDCall.FindAllString(line, -1))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return calls, comments
}

func TestUnpricedZeroCallerCensus(t *testing.T) {
	calls, comments := sweepCostUSDCallers(t)

	// Non-vacuity: a sweep that finds nothing satisfies every comparison below.
	total := 0
	for _, n := range calls {
		total += n
	}
	if total < 8 {
		t.Fatalf("the sweep found %d alerts.CostUSD call sites, want >= 8 — the walk is broken, "+
			"and a broken walk reports a clean census", total)
	}

	for file, n := range calls {
		rec, ok := remainingCallers[file]
		if ok && rec.sites != n {
			t.Errorf("%s has %d alerts.CostUSD call site(s); W6.20's census records %d.\n"+
				"    A call ADDED to an already-listed file inherits the zero just as silently as "+
				"one in a new file — say what the new site reads.", file, n, rec.sites)
		}
		if !ok {
			t.Errorf("%s calls the plain alerts.CostUSD and W6.20's census does not list it.\n"+
				"    That call books, displays or DECIDES on exactly 0 for any model the catalog "+
				"does not hold. Say what reads the zero and whether a fallback belongs there — "+
				"the answer is different for a settle, a gate, a display and a payout.", file)
		}
	}
	for file := range remainingCallers {
		if _, ok := calls[file]; !ok {
			t.Errorf("W6.20's census lists %s and the sweep finds no call there. If it was "+
				"repaired, record WHICH direction was chosen and why — a census that still names "+
				"a fixed site reads as an open finding.", file)
		}
	}

	// The comment-only mentions are excluded deliberately; prove the exclusion is
	// real rather than a walk that happened to miss them.
	for _, f := range commentMentions {
		if comments[f] == 0 {
			t.Errorf("%s was recorded as a COMMENT-only mention of alerts.CostUSD and the sweep "+
				"finds none there — the exclusion list has drifted from the code", f)
		}
		if calls[f] != 0 {
			t.Errorf("%s is on the comment-only list but has %d real call site(s)", f, calls[f])
		}
	}

	if t.Failed() {
		var lines []string
		for f, n := range calls {
			lines = append(lines, f+" ×"+strings.Repeat("I", n))
		}
		sort.Strings(lines)
		t.Logf("call sites found:\n  %s", strings.Join(lines, "\n  "))
	}
}

// ⚠ THE REASON W6.20 REPAIRS NOTHING, MADE INTO A NUMBER RATHER THAN AN ASSERTION.
// If the two fallback directions were close, "pick one" would be a small call. They
// are not: for the same request, PurposeHold falls back to the provider's most
// expensive known model and PurposeCharge to its cheapest.
func TestTheTwoFallbackDirectionsAreNotClose(t *testing.T) {
	const model = "gpt-4" // absent from the catalog
	if _, _, ok := catalog.Price(model); ok {
		t.Skipf("%s is now priced — the fallback distinction is not observable on it", model)
	}
	charge, cprov := CostUSDResolved(model, catalog.PurposeCharge, 1000, 0, 0, 1000)
	hold, hprov := CostUSDResolved(model, catalog.PurposeHold, 1000, 0, 0, 1000)

	if cprov != catalog.ProvenanceFallback || hprov != catalog.ProvenanceFallback {
		t.Fatalf("expected both to be fallbacks, got charge=%v hold=%v — the premise has changed",
			cprov, hprov)
	}
	if charge <= 0 || hold <= 0 {
		t.Fatalf("a fallback priced at 0 (charge=%v hold=%v) — then there is no direction to "+
			"choose and this test is measuring nothing", charge, hold)
	}
	if hold <= charge {
		t.Errorf("hold (%v) is not dearer than charge (%v) — W6.20's argument that the direction "+
			"is a real decision rests on this", hold, charge)
	}
	t.Logf("MEASURED, on 1000 in / 1000 out for %q: charge fallback %v, hold fallback %v — a "+
		"factor of %.0f. Choosing between them on a surface with no twin that has already "+
		"decided is setting a price.", model, charge, hold, hold/charge)
}
