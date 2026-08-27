package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// metric_census_test.go — declared, registered, operated, and named in a shipped
// alert. The gaps between those four populations.
//
// ⚠ THE CLASS IS THE FIRST ONE THIS QUEUE'S STANDARD NAMES: "a metric structurally
// zero and reported as measured". A counter that is declared and REGISTERED appears
// on /metrics with the value 0 — indistinguishable from "this never happened" —
// whether the truth is "this never happened" or "nothing can ever increment this".
//
// ⚠ THE MIRROR IS WORSE AND QUIETER: an alert rule or dashboard naming a metric
// nothing declares never fires, and nobody notices a rule that cannot fire.
//
// MEASURED HERE: 62 declared metrics, 61 of them operated somewhere in non-test Go,
// and every lens_* name in deploy/observability resolves to a declared metric once
// Prometheus's own histogram suffixes are accounted for.

var (
	metricDecl = regexp.MustCompile(`(?m)^\s*([A-Z][A-Za-z0-9_]*)\s*=\s*prometheus\.New(\w+)\(`)
	metricName = regexp.MustCompile(`Name:\s*"([a-z0-9_]+)"`)
	lensMetric = regexp.MustCompile(`\b(lens_[a-z0-9_]+)\b`)
	// An OPERATION, not a mention. Registration is not an operation: MustRegister
	// makes a metric appear on /metrics, which is exactly how a structurally-zero
	// series gets published in the first place.
	metricOp = `(?:\.Inc\(|\.Add\(|\.Set\(|\.Observe\(|\.Dec\(|\.WithLabelValues\(|\.With\(|\.SetToCurrentTime\()`
)

// ⚠ REGISTERED AND NEVER OPERATED — A RECORD, NOT AN APPROVAL.
// Each entry is a metric this binary publishes that no code path can move off zero.
// A reader scraping Lens sees the series and reads it as a measurement.
var structurallyZero = map[string]string{
	"TokensSavedTotal": "`lens_tokens_saved_total` — labels provider+strategy, help \"Tokens " +
		"saved, by provider and strategy.\" Declared at metrics.go and REGISTERED in the " +
		"MustRegister block, so it is published; nothing anywhere calls Inc/Add on it. ⚠ AND " +
		"\"tokens saved\" IS THE PRODUCT'S CENTRAL CLAIM — an operator scraping Lens to ask how " +
		"much it has saved gets a permanent 0 that reads exactly like \"nothing\". ⚠ Its sibling " +
		"DistillTokensSavedTotal IS operated, which is what makes this look wired rather than " +
		"pending. ⚠ NOT FIXED BY W6.24 AND DELIBERATELY SO: deciding what counts as a saving, " +
		"and under which `strategy` label, is a measurement decision, and publishing a WRONG " +
		"number is worse than publishing a zero. Bounded honestly: no shipped dashboard or alert " +
		"references it, so nothing currently DISPLAYS the false zero.",
}

// suffixed strips the series suffixes Prometheus derives from a histogram or summary
// — they are generated, not declared, and counting them as undeclared is a lie about
// the artefact rather than about the code.
//
// ⚠ IT MUST ONLY EVER BE TRIED AS A FALLBACK, AND THE FIRST DRAFT DID NOT DO THAT.
// `lens_ha_instance_count` is a real, declared GAUGE whose name genuinely ends in
// `_count`; stripping first turned it into `lens_ha_instance`, which nothing
// declares, and the guard reported a live alert rule as firing on a metric that does
// not exist. The exact name is checked first now. A mechanical exclusion that eats a
// real name is worse than no exclusion — it manufactures a finding.
func suffixed(name string) string {
	for _, s := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, s) {
			return strings.TrimSuffix(name, s)
		}
	}
	return name
}

func metricsRepoRoot(t *testing.T) string {
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

// declaredMetrics returns Go identifier → exported metric name.
func declaredMetrics(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	dir := filepath.Join(root, "internal/metrics")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/metrics: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, n))
		if rerr != nil {
			t.Fatalf("read %s: %v", n, rerr)
		}
		src := string(raw)
		for _, loc := range metricDecl.FindAllStringSubmatchIndex(src, -1) {
			ident := src[loc[2]:loc[3]]
			end := loc[1] + 700
			if end > len(src) {
				end = len(src)
			}
			name := ""
			if m := metricName.FindStringSubmatch(src[loc[1]:end]); m != nil {
				name = m[1]
			}
			out[ident] = name
		}
	}
	return out
}

// registeredMetrics returns the identifiers inside the MustRegister call — the ones
// that actually reach /metrics.
func registeredMetrics(t *testing.T, root string, declared map[string]string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "internal/metrics/metrics.go"))
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "prometheus.MustRegister(")
	if i < 0 {
		t.Fatal("no prometheus.MustRegister( in metrics.go — nothing is published, and every " +
			"structurally-zero claim below would be about a series nobody can scrape")
	}
	depth, end := 0, len(src)
	for j := i + len("prometheus.MustRegister"); j < len(src); j++ {
		if src[j] == '(' {
			depth++
		} else if src[j] == ')' {
			depth--
			if depth == 0 {
				end = j
				break
			}
		}
	}
	block := src[i:end]
	out := map[string]bool{}
	for ident := range declared {
		if regexp.MustCompile(`\b` + ident + `\b`).MatchString(block) {
			out[ident] = true
		}
	}
	return out
}

// operatedMetrics returns identifiers on which some non-test code performs an
// operation — never a mention, and never a registration.
func operatedMetrics(t *testing.T, root string, declared map[string]string) map[string]string {
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
		src := string(raw)
		for ident := range declared {
			if _, done := out[ident]; done {
				continue
			}
			if regexp.MustCompile(`\b`+ident+metricOp).MatchString(src) ||
				regexp.MustCompile(`\b`+ident+`\s*\)\s*`+metricOp).MatchString(src) {
				out[ident] = rel
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return out
}

func TestEveryRegisteredMetricCanLeaveZero(t *testing.T) {
	root := metricsRepoRoot(t)
	declared := declaredMetrics(t, root)
	if len(declared) < 50 {
		t.Fatalf("found %d declared metrics, want >= 50 — the parse has broken, and a broken "+
			"parse finds no structurally-zero series", len(declared))
	}
	registered := registeredMetrics(t, root, declared)
	if len(registered) < 40 {
		t.Fatalf("found %d registered metrics, want >= 40 — the MustRegister parse has broken",
			len(registered))
	}
	operated := operatedMetrics(t, root, declared)
	if len(operated) < 40 {
		t.Fatalf("found %d operated metrics, want >= 40 — the operation sweep has broken, and a "+
			"broken sweep calls every metric structurally zero", len(operated))
	}

	var zeros []string
	for ident := range declared {
		if !registered[ident] {
			continue // declared but not published: it reaches no scrape, so it is not this class
		}
		if _, ok := operated[ident]; ok {
			continue
		}
		zeros = append(zeros, ident)
	}
	sort.Strings(zeros)

	for _, z := range zeros {
		if _, ok := structurallyZero[z]; !ok {
			t.Errorf("%s (%s) is REGISTERED and never operated — it is published on /metrics as a "+
				"permanent 0, which reads exactly like \"this never happened\".\n"+
				"    Either wire it, stop registering it, or record it in structurallyZero with "+
				"the reason and who decides.", z, declared[z])
		}
	}
	for want := range structurallyZero {
		found := false
		for _, z := range zeros {
			if z == want {
				found = true
			}
		}
		if !found {
			t.Errorf("W6.24 records %s as registered-but-never-operated and it no longer is. If it "+
				"was wired, delete the record and say what now increments it — a record claiming a "+
				"live metric is dead is worse than none.", want)
		}
	}
	t.Logf("%d declared, %d registered, %d operated; %d registered-and-never-operated: %v",
		len(declared), len(registered), len(operated), len(zeros), zeros)
}

// ⚠ THE MIRROR. An alert or dashboard naming a metric nothing declares never fires.
func TestEveryAlertedMetricIsDeclared(t *testing.T) {
	root := metricsRepoRoot(t)
	declared := declaredMetrics(t, root)
	names := map[string]bool{}
	for _, n := range declared {
		if n != "" {
			names[n] = true
		}
	}
	if len(names) < 50 {
		t.Fatalf("only %d declared metric NAMES parsed, want >= 50 — Name: is not being read, and "+
			"every alerted name would look undeclared", len(names))
	}

	obs := filepath.Join(root, "deploy/observability")
	if _, err := os.Stat(obs); err != nil {
		t.Skipf("no deploy/observability tree: %v", err)
	}
	// ⚠ NOT EVERY lens_* TOKEN IN THESE FILES IS A METRIC. `lens_token_balances` appears
	// in a runbook sentence about Postgres lock contention — it is a TABLE. Prose is
	// excluded by requiring the name to appear in a rule/dashboard expression file, and
	// the runbook markdown is swept only for names that also parse as metrics.
	var ghosts []string
	seen := map[string]bool{}
	err := filepath.Walk(obs, func(path string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			return nil // runbook prose — see the note above
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range lensMetric.FindAllStringSubmatch(string(raw), -1) {
			// Exact name FIRST; the generated-suffix strip is only a fallback.
			if names[m[1]] || names[suffixed(m[1])] || seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			ghosts = append(ghosts, m[1]+"  in "+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk observability: %v", err)
	}
	if len(ghosts) > 0 {
		sort.Strings(ghosts)
		t.Errorf("%d metric name(s) in shipped alert/dashboard files are declared by nothing:\n  %s\n"+
			"    A rule on a metric that does not exist never fires, and nobody notices a rule "+
			"that cannot fire.", len(ghosts), strings.Join(ghosts, "\n  "))
	}
}

// ⚠ TEETH. Both tests assert a residue is empty or exactly-recorded; a sweep that
// found nothing would satisfy both.
func TestMetricCensusCanActuallyClassify(t *testing.T) {
	root := metricsRepoRoot(t)
	declared := declaredMetrics(t, root)
	operated := operatedMetrics(t, root, declared)

	if _, ok := declared["RequestsTotal"]; !ok {
		t.Error("RequestsTotal is not seen as declared — the declaration parse is blind")
	}
	if _, ok := operated["RequestsTotal"]; !ok {
		t.Error("RequestsTotal is not seen as operated — the operation sweep is blind, and a " +
			"blind sweep calls every metric structurally zero")
	}
	if _, ok := operated["TokensSavedTotal"]; ok {
		t.Error("TokensSavedTotal is now seen as operated — either it was wired (update the " +
			"record) or the sweep counts something that is not an operation")
	}
	if suffixed("lens_x_bucket") != "lens_x" || suffixed("lens_y") != "lens_y" {
		t.Error("the histogram-suffix stripper is wrong; _bucket/_sum/_count are generated by " +
			"Prometheus and are not declarations")
	}
	// ⚠ AND THE STRIPPER MUST NOT BE ALLOWED TO EAT A REAL NAME. lens_ha_instance_count
	// is a declared gauge whose name ends in _count; the first draft stripped before
	// checking and manufactured a finding out of a working alert rule.
	names := map[string]bool{}
	for _, n := range declared {
		if n != "" {
			names[n] = true
		}
	}
	if !names["lens_ha_instance_count"] {
		t.Fatal("lens_ha_instance_count is no longer declared — this control has lost its subject")
	}
	if names[suffixed("lens_ha_instance_count")] {
		t.Error("suffixed() maps a declared _count metric onto another declared name; the " +
			"exact-name-first order is the only thing keeping that from manufacturing findings")
	}
}
