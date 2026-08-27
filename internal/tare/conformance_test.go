package tare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// conformance_test.go — the properties EVERY reducer in this package must have, applied to every
// reducer in this package.
//
// ⚠ WHY THIS EXISTS WHEN THE EXISTING TESTS ARE GOOD. They are: json_test, gocode_test and
// prefix_test each prove determinism, safe refusal, size-monotonicity and (for JSON) a real
// round-trip through tare.ExpandJSON over this repository's own corpora. What none of them does is
// make those PACKAGE properties. They are three independent sets of per-reducer tests, so a FOURTH
// reducer inherits nothing — it can be non-deterministic, silently lossy, or larger than its input,
// and every existing test stays green because none of them has heard of it.
//
// That matters now rather than later. W6.1 names three phase-1 reductions — "JSON dedup,
// tree-sitter AST trimming, log collapse". Two are built. ⚠ THE UNBUILT ONE IS THE LOSSY KIND:
// collapsing repeated log lines DROPS content, which is exactly where "keep the numbers, paths and
// IDs" stops being decoration and starts being the thing that makes the output trustworthy.
//
// ⚠ AND THAT RULE IS NOT IN THIS TREE. W6.1 quotes the design requiring "hardcoded must-keep for
// numbers/paths/IDs". Measured: no implementation, and no mention in any of the four
// docs/tare-phase1*.md files. It is MOOT for what ships today — the JSON reducer drops nothing and
// the Go trimmer announces every elision — so this is a gap in the RECORD, not a live defect. It is
// written down here because the reducer that makes it live is the next one.

// ── the registry ────────────────────────────────────────────────────────────────────────────────

// conformer is one reducer under test, with the properties it CLAIMS. The claims are what the
// tests hold it to; a reducer cannot opt out by staying quiet, because
// TestEveryReductionInThePackageIsRegistered fails when a Reduce method exists with no entry here.
type conformer struct {
	name string
	kind tare.Kind
	make func() tare.Reduction

	// lossy: content that goes in cannot be recovered from what comes out.
	lossy bool
	// marker is what a LOSSY reducer must put in the output wherever it dropped something. A lossy
	// reducer with no marker returns a smaller truth as if it were the whole one.
	marker string
	// inverse recovers the input. ⚠ REQUIRED FOR ANY REDUCER CLAIMING TO BE LOSSLESS: a
	// losslessness nobody can demonstrate is a claim, not a property.
	inverse func([]byte) ([]byte, error)

	// samples must ACTUALLY reduce. Checked, so a conformer cannot pass by handing every test an
	// input its reducer declines to touch.
	samples [][]byte
	// wrongKind is content this reducer must refuse, to exercise the refusal contract.
	wrongKind []byte
}

func jsonSamples() [][]byte {
	return [][]byte{
		[]byte(`{"items":[{"id":1,"name":"a","ok":true},{"id":2,"name":"b","ok":true},{"id":3,"name":"c","ok":true}]}`),
		// ⚠ THE FIRST DRAFT OF THIS SAMPLE WAS `[{"i":0,"s":"ok"},…]` AND IT DID NOT REDUCE — the
		// keys are shorter than the table header that would replace them, so the reducer correctly
		// declined and every property below became vacuous on it. TestConformance_SamplesActuallyReduce
		// caught that on its first run, which is the whole reason it is the first test in the file.
		[]byte(`[{"index":0,"status":"ok","message":"FIRST"},{"index":1,"status":"ok","message":"x"},` +
			`{"index":2,"status":"error","message":"the failure an agent is looking for"},` +
			`{"index":3,"status":"ok","message":"LAST"}]`),
	}
}

const goSample = `package p

import "fmt"

type T struct{ A int }

func F(x int) (int, error) {
	if x > 0 {
		fmt.Println("deep/path/id-42")
	}
	return x * 2, nil
}
`

func conformers() []conformer {
	return []conformer{
		{
			name: "JSONReducer", kind: tare.KindJSON,
			make:    func() tare.Reduction { return tare.NewJSONReducer() },
			lossy:   false,
			inverse: tare.ExpandJSON,
			samples: jsonSamples(),
			// Valid JSON with no tableable array: refused for a reason, not by erroring.
			wrongKind: []byte(`{"a":1}`),
		},
		{
			name: "GoBodyTrimmer", kind: tare.KindCode,
			make:      func() tare.Reduction { return tare.NewGoBodyTrimmer() },
			lossy:     true,
			marker:    tare.ElisionMarker,
			samples:   [][]byte{[]byte(goSample)},
			wrongKind: []byte("this is not Go source at all"),
		},
	}
}

// ── the census that makes the registry unbypassable ─────────────────────────────────────────────

var reduceMethod = regexp.MustCompile(`(?m)^func \(\w+ \*?(\w+)\) Reduce\(`)

// ⚠ WITHOUT THIS, EVERY TEST BELOW IS OPT-IN. A new reducer that simply is not added to
// conformers() would be held to nothing at all, and the suite would report a clean run — the exact
// shape of guard this queue keeps finding: green because it was never pointed at anything.
func TestEveryReductionInThePackageIsRegistered(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := map[string]bool{}
	var scanned int
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(dir, n))
		if rerr != nil {
			t.Fatalf("read %s: %v", n, rerr)
		}
		scanned++
		for _, m := range reduceMethod.FindAllStringSubmatch(string(src), -1) {
			found[m[1]] = true
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no production files — a broken scan finds no unregistered reducers and " +
			"reports a clean registry")
	}
	if len(found) < 2 {
		t.Fatalf("found %d Reduce method(s) in %d file(s); this package has at least three — the "+
			"regex is not matching and the census is inert", len(found), scanned)
	}

	prefixTests, err := os.ReadFile("prefix_test.go")
	if err != nil {
		t.Fatalf("read prefix_test.go: %v — the PrefixStable exclusion below rests on it", err)
	}

	registered := map[string]bool{}
	for _, c := range conformers() {
		registered[c.name] = true
	}
	// PrefixStable is a WRAPPER, not a reducer: it delegates to an inner Reduction, and its own
	// properties are the ones that matter for it — a byte-identical prefix, and only the newest
	// message changing. Those are proved in prefix_test.go against a stub.
	//
	// ⚠ THE EXCLUSION IS CHECKED, NOT TRUSTED, AND W6.26 IS WHY. The tenant-erasure guard exempted
	// `workspaces` BY NAME and the exemption silently covered a category with a second member
	// nobody had written down. An exclusion that names a file without checking the file still says
	// what it claims is the same shape of promise. So: the tests that justify this one must exist.
	for _, must := range []string{
		"TestPrefix_TurnNAndTurnNPlusOneShareAByteIdenticalPrefix",
		"TestPrefix_OnlyTheNewestMessageChanges",
		"TestPrefix_RefusesAndReturnsTheInputUnchanged",
	} {
		if !bytes.Contains(prefixTests, []byte(must)) {
			t.Errorf("PrefixStable is excluded from the conformance registry because prefix_test.go "+
				"proves its properties directly, and %s is not there. Either restore it or bring "+
				"PrefixStable into conformers().", must)
		}
	}
	registered["PrefixStable"] = true

	var missing []string
	for name := range found {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d reducer(s) implement Reduce and are not in conformers(): %s\n\n"+
			"Every reducer in this package must declare whether it is lossy, what marker it leaves "+
			"where it dropped content, and — if it claims to be lossless — the inverse that "+
			"demonstrates it. A reducer held to none of those is one nobody can audit.",
			len(missing), strings.Join(missing, ", "))
	}
}

// ── the properties ──────────────────────────────────────────────────────────────────────────────

// ⚠ ARMED. Every property below is trivially true of a reducer that returns its input, so each
// sample is first proved to actually reduce. A conformance suite fed inert samples is the most
// comfortable kind of green there is.
func TestConformance_SamplesActuallyReduce(t *testing.T) {
	for _, c := range conformers() {
		for i, s := range c.samples {
			out, tin, tout, err := c.make().Reduce(context.Background(), s, c.kind)
			if err != nil {
				t.Errorf("%s sample %d: unexpected error %v", c.name, i, err)
				continue
			}
			if bytes.Equal(out, s) {
				t.Errorf("%s sample %d came back unchanged — every property asserted about this "+
					"sample below is vacuous", c.name, i)
			}
			if len(out) >= len(s) {
				t.Errorf("%s sample %d: %d bytes in, %d out — not a reduction", c.name, i, len(s), len(out))
			}
			if tout > tin {
				t.Errorf("%s sample %d: tokensOut %d > tokensIn %d", c.name, i, tout, tin)
			}
		}
	}
}

func TestConformance_IsDeterministic(t *testing.T) {
	for _, c := range conformers() {
		for i, s := range c.samples {
			a, _, _, _ := c.make().Reduce(context.Background(), s, c.kind)
			b, _, _, _ := c.make().Reduce(context.Background(), s, c.kind)
			if !bytes.Equal(a, b) {
				t.Errorf("%s sample %d is not deterministic. A reduction that varies run to run "+
					"cannot be cached, diffed, or held to a measured saving.\n  a: %s\n  b: %s",
					c.name, i, a, b)
			}
		}
	}
}

// ⚠ REFUSAL IS A CONTRACT, NOT AN ERROR. tare.Reduction's doc says refusal is `reduced == content,
// err == nil` — "the compressor that shipped before had no way to decline". A reducer that returns
// an error where it means "not mine" makes every caller treat a normal outcome as a failure.
func TestConformance_RefusalReturnsTheInputUnchangedWithNilError(t *testing.T) {
	for _, c := range conformers() {
		out, tin, tout, err := c.make().Reduce(context.Background(), c.wrongKind, c.kind)
		if err != nil {
			t.Errorf("%s refused with err=%v; refusal must be (content, nil)", c.name, err)
		}
		if !bytes.Equal(out, c.wrongKind) {
			t.Errorf("%s refused but did not return the input unchanged.\n  in:  %s\n  out: %s",
				c.name, c.wrongKind, out)
		}
		if tin != tout {
			t.Errorf("%s refused and reported tokensIn=%d tokensOut=%d — a refusal saved nothing "+
				"and must not book a saving", c.name, tin, tout)
		}
	}
}

// ⚠ A REDUCER THAT DECLARES ITSELF LOSSLESS MUST SHIP THE INVERSE THAT PROVES IT. This is the
// property the whole economy rests on: a saving booked against content that cannot be recovered is
// a saving nobody can audit.
func TestConformance_LosslessReducersRoundTrip(t *testing.T) {
	for _, c := range conformers() {
		if c.lossy {
			continue
		}
		if c.inverse == nil {
			t.Errorf("%s claims to be lossless and ships no inverse. Losslessness that cannot be "+
				"demonstrated is a claim, not a property — either add the inverse or declare the "+
				"reducer lossy and give it a marker", c.name)
			continue
		}
		for i, s := range c.samples {
			out, _, _, err := c.make().Reduce(context.Background(), s, c.kind)
			if err != nil {
				t.Errorf("%s sample %d: %v", c.name, i, err)
				continue
			}
			restored, rerr := c.inverse(out)
			if rerr != nil {
				t.Errorf("%s sample %d: inverse failed: %v", c.name, i, rerr)
				continue
			}
			var want, got any
			if err := json.Unmarshal(s, &want); err != nil {
				t.Fatalf("%s sample %d is not JSON: %v", c.name, i, err)
			}
			if err := json.Unmarshal(restored, &got); err != nil {
				t.Errorf("%s sample %d: the expanded form is not valid JSON: %v", c.name, i, err)
				continue
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("%s sample %d does not round-trip.\n  in:       %s\n  restored: %s",
					c.name, i, s, restored)
			}
		}
	}
}

// ⚠ A LOSSY REDUCER MUST SAY WHERE IT DROPPED SOMETHING. gocode.go states the reason and it
// generalises: a body replaced by `{}` tells a reading agent the function IS EMPTY — "not a smaller
// truth, a different and false one". Any future lossy reducer inherits that obligation here.
func TestConformance_LossyReducersAnnounceEveryElision(t *testing.T) {
	for _, c := range conformers() {
		if !c.lossy {
			continue
		}
		if c.marker == "" {
			t.Errorf("%s is declared lossy and names no marker — its output cannot be told apart "+
				"from content that was simply that short", c.name)
			continue
		}
		for i, s := range c.samples {
			out, _, _, err := c.make().Reduce(context.Background(), s, c.kind)
			if err != nil || bytes.Equal(out, s) {
				continue // refused: nothing dropped, nothing to announce
			}
			if !bytes.Contains(out, []byte(c.marker)) {
				t.Errorf("%s sample %d shrank %d→%d bytes and the output carries no %q. Content was "+
					"dropped and the output does not say so.", c.name, i, len(s), len(out), c.marker)
			}
		}
	}
}

// ── phase-1 completeness, recorded rather than assumed ──────────────────────────────────────────

// ⚠ W6.1 NAMES THREE PHASE-1 REDUCTIONS AND TWO ARE BUILT. KindLog and KindProse are declared in
// tare.go with no reducer behind them, so a caller that routes on Kind has two branches that
// resolve to nothing. This is not a defect — phases ship in order — but an unbuilt reducer and a
// forgotten one look identical from outside, and this is what tells them apart.
//
// ⚠ IT IS ALSO THE TRIPWIRE FOR THE must-keep GAP. The log reducer is the lossy one; the day it
// exists, this test fails and the failure message is where "keep the numbers, paths and IDs" has to
// be answered.
func TestDeclaredKindsWithoutAReducer(t *testing.T) {
	unbuilt := []tare.Kind{tare.KindLog, tare.KindProse}
	built := map[tare.Kind]bool{}
	for _, c := range conformers() {
		built[c.kind] = true
	}
	if len(built) < 2 {
		t.Fatal("fewer than two kinds have a reducer — the registry is not describing this package")
	}
	for _, k := range unbuilt {
		if built[k] {
			t.Errorf("Kind %q now has a reducer and this record says it does not.\n\n"+
				"    If it is LOSSY — and log collapse is — then W6.1's \"hardcoded must-keep for "+
				"numbers/paths/IDs\" applies to it. That rule is not implemented anywhere in this "+
				"tree and appears in none of the four docs/tare-phase1*.md files (measured, W6.28). "+
				"Answer it here rather than inheriting the silence.", k)
		}
	}
}
