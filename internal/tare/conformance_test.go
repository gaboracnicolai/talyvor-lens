package tare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
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
// tree-sitter AST trimming, log collapse". Two are built. The third is the next one.
//
// ⚠⚠ AND WHAT THIS FILE USED TO SAY ABOUT THAT THIRD ONE WAS WRONG IN BOTH HALVES, CORRECTED HERE
// AGAINST THE DESIGN ITSELF RATHER THAN AGAINST THE QUEUE'S SUMMARY OF IT. It said "THE UNBUILT ONE
// IS THE LOSSY KIND: collapsing repeated log lines DROPS content", and that W6.1's "hardcoded
// must-keep for numbers/paths/IDs" was a phase-1 rule this tree had failed to implement. Read from
// ~/talyvor-queue/reports/tare-design-v1.html — which is NOT in any repository, and that is the root
// cause of this drift — neither is what the design says.
//
// (1) "THE UNBUILT ONE IS THE LOSSY KIND" WAS STATED AS A FACT ABOUT A REDUCER NOBODY HAS WRITTEN,
// AND THE DESIGN LEANS THE OTHER WAY:
//
//	§03 architecture diagram: "ContentRouter → JSON dedup · code AST (tree-sitter)
//	                           · log-collapse [lossless first]"
//	§03 prose:                "Lossless-first content routing. v1 ships only reductions that cannot
//	                           silently drop signal an enterprise agent needs: JSON de-duplication,
//	                           tree-sitter AST structural trimming (output stays syntactically
//	                           valid), log collapse."
//	roadmap, go-live-aligned: "Lossless structural reduction (Phase 1) + the metering surface."
//	roadmap, phase 2+:        "the aggressive lossy path and reversible cache"
//
// ⚠ AND THE HONEST READING IS NOT "LOSSY IS FORBIDDEN", WHICH IS WHERE A FIRST DRAFT OF THIS
// CORRECTION OVERSHOT. The operative phrase is "cannot SILENTLY drop signal", and this package
// already ships a phase-1 reducer that is lossy and acceptable: the Go body trimmer drops bodies
// and announces every elision, which docs/tare-phase1b-measured.md argues satisfies exactly that
// criterion. So the design's own Phase-1 label ("Lossless structural reduction") is already looser
// in practice than it reads, and a lossy-but-announced log reducer is a legitimate build.
//
// What was wrong was asserting the outcome in advance, in a failure message that the person about
// to write the reducer will read as an instruction. Lossless is the design's stated starting point
// ("[lossless first]"); collapsing a run of identical lines into one line plus a count is
// reversible, so it is reachable. Whether it ends up lossless is a build decision governed by this
// registry's existing contract — a lossless reducer owes a demonstrated inverse, a lossy one owes a
// marker on every elision — not something this comment should have pre-decided.
//
// (2) "HARDCODED MUST-KEEP FOR NUMBERS/PATHS/IDS" IS NOT A PHASE-1 RULE — IT BELONGS TO A PHASE-2
// COMPONENT THAT DOES NOT EXIST YET. In the design it is a parenthetical glossing the word
// "extractive" inside ONE ROW of the sourcing table, whose columns are Piece / Call / Why:
//
//	Piece: "Text model ModernBERT dual-head · extractive"   Call: "Vendor base + build LoRA"
//	Why:   "Extractive (keeps a subset of original tokens in order, never rephrases; hardcoded
//	        must-keep for numbers/paths/IDs) — enterprise-safe. …"
//
// It is the mitigation for an ML model that SELECTS A SUBSET OF TOKENS and could otherwise drop an
// identifier. §07 attributes it the same way — "THE MODEL is extractive (never rephrases)" — and
// the ML prose path is "phase 2, gated" by §03's own words.
//
// This package already reached the same conclusion once, for the other rule, and then did not apply
// it here: docs/tare-phase1a-measured.md records that W6.1.1's "KEEP errors, outliers, and
// first-and-last elements" "CANNOT FAIL" under a lossless transform because nothing is dropped at
// all. A must-keep rule has the same shape. It binds a reducer that CHOOSES what to discard; it is
// satisfied by construction by one that discards nothing, and it is satisfied by announcement by
// one that says what it discarded.
//
// ⚠ WHY THE DRIFT HAPPENED, BECAUSE IT WILL HAPPEN AGAIN: the design lives in NO REPOSITORY (a
// defect this queue has recorded twice and nobody has fixed), so every session re-quotes it from a
// previous session's paraphrase. Two hops turned a phase-2 model mitigation into a phase-1 blocker,
// and a "[lossless first]" annotation into "the lossy kind". Nothing was careless; the source was
// simply not reachable from the tree. The quotes above are reproduced verbatim so the next reader
// can check the claim without the HTML.

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
	// equal decides whether the restored bytes match the input. ⚠ NIL MEANS BYTE-FOR-BYTE, WHICH IS
	// THE STRICTER CHECK AND THE RIGHT DEFAULT. Only a reducer whose inverse legitimately cannot
	// reproduce the original bytes — JSON, where key order and whitespace are not content — supplies
	// a looser one, and supplying it is a visible act rather than a property of the harness.
	//
	// ⚠ THIS FIELD EXISTS BECAUSE THE HARNESS USED TO COMPARE EVERY ROUND-TRIP BY PARSING BOTH SIDES
	// AS JSON, AND `t.Fatalf`d ON ANYTHING ELSE. Measured with a synthetic lossless conformer over
	// log lines — the next phase-1 reducer's own content — the failure was
	// "ProbeLosslessNonJSON sample 0 is not JSON: invalid character 'E'", which says nothing about
	// the reducer and everything about the harness. Worse, `t.Fatalf` aborts the test, so every
	// conformer after it went unchecked too. A conformance suite that only admits JSON is not a
	// conformance suite; it is the JSON reducer's own test with a wider name.
	equal func(orig, restored []byte) bool

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

// logSamples are two shapes a real log actually takes: a retry storm, and a flapping health check
// with unrelated traffic between the runs.
//
// ⚠ BOTH MUST ACTUALLY REDUCE — TestConformance_SamplesActuallyReduce checks it, and it is the
// check that caught the first JSON sample being one the reducer correctly declined. Lines here are
// long enough that hiding copies pays for the marker; that is not a fixture convenience, it is the
// same arithmetic worthCollapsing does, and a sample of short lines would be refused exactly as a
// short-line log is.
func logSamples() [][]byte {
	const dial = "2026-08-28T01:02:03Z ERROR db: connection refused dialing 10.0.0.7:5432 (attempt 14)\n"
	const probe = "2026-08-28T01:04:00Z WARN health: probe /readyz timed out after 2s (consecutive 31)\n"
	return [][]byte{
		[]byte("2026-08-28T01:02:02Z INFO db: dialing primary\n" + strings.Repeat(dial, 12) +
			"2026-08-28T01:02:09Z INFO db: connected to 10.0.0.8:5432\n"),
		[]byte(strings.Repeat(probe, 5) +
			"2026-08-28T01:04:11Z INFO health: probe /readyz ok\n" + strings.Repeat(probe, 7)),
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

// jsonSemanticEqual compares two JSON documents by value. It is the LOOSER of the two comparators
// and is opt-in for exactly that reason: it accepts a restored form whose bytes differ, which is
// correct for JSON and would be a hole anywhere else.
//
// ⚠ It refuses rather than passes when either side will not parse — a comparator that cannot read
// its inputs must not report them equal.
func jsonSemanticEqual(orig, restored []byte) bool {
	var a, b any
	if err := json.Unmarshal(orig, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(restored, &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func conformers() []conformer {
	return []conformer{
		{
			name: "JSONReducer", kind: tare.KindJSON,
			make:    func() tare.Reduction { return tare.NewJSONReducer() },
			lossy:   false,
			inverse: tare.ExpandJSON,
			// JSON is the one place where byte-equality is the WRONG question: key order and
			// whitespace are not content, and ExpandJSON re-encodes rather than replaying bytes.
			equal:   jsonSemanticEqual,
			samples: jsonSamples(),
			// Valid JSON with no tableable array: refused for a reason, not by erroring.
			wrongKind: []byte(`{"a":1}`),
		},
		{
			name: "LogCollapse", kind: tare.KindLog,
			make:  func() tare.Reduction { return tare.NewLogCollapse() },
			lossy: false,
			// LOSSLESS, and demonstrated rather than declared: ExpandLog puts the bytes back and
			// the comparison below is byte-for-byte (no `equal` override — logs are not JSON and
			// nothing about a log line is "not content").
			inverse: tare.ExpandLog,
			samples: logSamples(),
			// A log with no repeated run: refused for a reason, not by erroring.
			wrongKind: []byte("alpha\nbeta\ngamma\n"),
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

// reducerReceiversIn returns the receiver type name of every method named Reduce declared in src.
//
// ⚠ IT PARSES, IT DOES NOT MATCH. The previous version was a regex over the source text and it was
// wrong in both directions — see TestReducerCensusReadsDeclarationsNotSpellings for the measured
// arms. A parser is what tells a DECLARATION apart from the same characters inside a comment or a
// string, and it is what sees a receiver whose name the method does not bother to give.
//
// ⚠ IT KEYS ON THE METHOD NAME ONLY, NOT ON THE SIGNATURE, AND THAT IS THE CONSERVATIVE DIRECTION.
// A method named Reduce whose signature does not satisfy tare.Reduction is reported and must then
// be registered or renamed — noisy. Matching the signature instead would let a reducer that is one
// type away from the interface pass unseen, which is the failure that matters.
//
// ⚠ A PARSE ERROR IS RETURNED, NEVER SWALLOWED. A scanner that returns nothing agrees with any
// registry.
func reducerReceiversIn(filename string, src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name == nil || fd.Name.Name != "Reduce" {
			continue
		}
		if n := receiverTypeName(fd); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// receiverTypeName is the bare type name a method is declared on, or "" for a plain function.
//
// ⚠ THE UNWRAPPING IS THE POINT. `*T`, `T[P]` and `*T[K, V]` are all ordinary ways to spell a
// receiver and each defeated the regex this replaced; the receiver may also have NO name at all,
// which is idiomatic Go for a method that does not use it and is invisible to any pattern that
// expects an identifier before the type.
func receiverTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return ""
	}
	expr := fd.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr: // *T
			expr = t.X
		case *ast.IndexExpr: // T[P]
			expr = t.X
		case *ast.IndexListExpr: // T[K, V]
			expr = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

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
		recvs, perr := reducerReceiversIn(n, src)
		if perr != nil {
			t.Fatalf("parse %s: %v — a file this census cannot read is a file it cannot police", n, perr)
		}
		for _, r := range recvs {
			found[r] = true
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no production files — a broken scan finds no unregistered reducers and " +
			"reports a clean registry")
	}
	if len(found) < 2 {
		t.Fatalf("found %d Reduce method(s) in %d file(s); this package has at least three — the "+
			"scan is not finding them and the census is inert", len(found), scanned)
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
			eq := c.equal
			if eq == nil {
				eq = bytes.Equal // strict by default; see conformer.equal
			}
			if !eq(s, restored) {
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
// ⚠⚠ AND IT NOW ENFORCES THE PHASE BOUNDARY RATHER THAN NARRATING IT. The previous version of this
// test asserted that KindLog was absent and, in its failure message, told whoever built it that log
// collapse "is LOSSY" and therefore owed a must-keep rule. Both halves were wrong against the
// design (see the correction at the top of this file), and a failure message is a build
// instruction — it is read by exactly the person about to write the reducer. So the rule is now:
//
//	KindProse — must stay absent. The prose reducer IS the ML text model, which §03 calls
//	            "phase 2, gated". Building it here skips that gate. This is the one part of the
//	            phase boundary the design states flatly, so it is the one part enforced here.
//	KindLog   — MAY exist, lossless or lossy. NOT constrained here on purpose: the design's
//	            criterion is "cannot SILENTLY drop signal", and the Go body trimmer already
//	            satisfies it while being lossy. The registry above is what holds a log reducer to
//	            its contract — a lossless one owes a demonstrated inverse, a lossy one owes a
//	            marker on every elision — and that is a stronger check than a phase label.
//
// ⚠ AN EARLIER DRAFT OF THIS RULE REJECTED A LOSSY KindLog OUTRIGHT AND THAT WAS AN OVERSHOOT of
// the same kind it was written to correct: reading "Lossless structural reduction (Phase 1)" as a
// prohibition, when the shipped phase-1 Go trimmer is lossy and docs/tare-phase1b-measured.md
// argues why that is within the design. A guard that encodes a stricter rule than its source is
// still a guard that lies, and it fails on correct work, which is how guards get deleted.
//
// ⚠ THE RULE IS A PURE FUNCTION BECAUSE OTHERWISE IT IS VACUOUS TODAY. With no KindLog reducer in
// the tree there is nothing for it to judge, and a guard that cannot fail on the current tree looks
// exactly like a guard that works. TestPhaseOneKindRuleIsNotVacuous drives it with synthetic
// registries — including the lossy KindLog it exists to reject — so the green below is earned.
func phaseOneKindProblems(cs []conformer) []string {
	built := map[tare.Kind]conformer{}
	for _, c := range cs {
		built[c.kind] = c
	}
	var problems []string
	if len(built) < 2 {
		return []string{"fewer than two kinds have a reducer — the registry is not describing this package"}
	}
	if c, ok := built[tare.KindProse]; ok {
		problems = append(problems, "Kind "+string(tare.KindProse)+" now has a reducer ("+c.name+
			"), and it must not. The prose reducer IS the ML text model, which the design calls "+
			`"phase 2, gated" — the gate is the extractive-labeling corpus and the lossless-first `+
			"decision holding up in real workloads. Building it here skips that gate rather than "+
			"passing it.")
	}
	return problems
}

func TestDeclaredKindsWithoutAReducer(t *testing.T) {
	for _, p := range phaseOneKindProblems(conformers()) {
		t.Error(p)
	}
}

// ⚠ POSITIVE CONTROL ON THE RULE ABOVE, WHICH JUDGES NOTHING IN TODAY'S TREE. Without this the
// phase boundary is enforced by a function that has never been shown to reject anything.
func TestPhaseOneKindRuleIsNotVacuous(t *testing.T) {
	ok := func(name string, k tare.Kind, lossy bool) conformer {
		return conformer{name: name, kind: k, lossy: lossy}
	}
	base := []conformer{ok("JSONReducer", tare.KindJSON, false), ok("GoBodyTrimmer", tare.KindCode, true)}

	cases := []struct {
		name    string
		cs      []conformer
		wantAny bool
		wantSub string
	}{
		{"today's registry — two kinds, neither log nor prose", base, false, ""},
		{"a LOSSLESS log reducer is accepted",
			append(append([]conformer{}, base...), ok("LogCollapse", tare.KindLog, false)), false, ""},
		{"a LOSSY log reducer is ALSO accepted — the registry, not this rule, holds it to its contract",
			append(append([]conformer{}, base...), ok("LogCollapse", tare.KindLog, true)), false, ""},
		{"a prose reducer is the ML text model and is gated",
			append(append([]conformer{}, base...), ok("ProseModel", tare.KindProse, false)), true, "phase 2, gated"},
		{"an empty registry describes nothing and must say so",
			nil, true, "not describing this package"},
	}
	for _, c := range cases {
		got := phaseOneKindProblems(c.cs)
		if c.wantAny && len(got) == 0 {
			t.Errorf("%s: rule accepted a registry it must reject — the phase boundary is not "+
				"enforced by anything", c.name)
			continue
		}
		if !c.wantAny && len(got) != 0 {
			t.Errorf("%s: rule rejected a registry it must accept: %v — a guard that fires on the "+
				"correct build gets deleted", c.name, got)
			continue
		}
		if c.wantSub != "" && !strings.Contains(strings.Join(got, " "), c.wantSub) {
			t.Errorf("%s: rejected, but the message does not say why (want it to contain %q): %v",
				c.name, c.wantSub, got)
		}
	}
}

// ── controls on the round-trip harness itself ───────────────────────────────────────────────────

// ⚠ THE HARNESS ONLY ADMITTED JSON, AND THE NEXT PHASE-1 REDUCER IS FOR LOGS. Measured before the
// fix with a synthetic lossless conformer over log lines: TestConformance_LosslessReducersRoundTrip
// died on "sample 0 is not JSON: invalid character 'E'" — a t.Fatalf, so every conformer after it
// went unchecked as well. These three cases pin the widened behaviour so it cannot narrow again.
//
// They drive the same comparison the round-trip test uses, rather than re-deriving it, so a change
// to that logic that these do not see is not possible.
func TestRoundTripComparisonIsByteExactByDefaultAndAdmitsNonJSON(t *testing.T) {
	compare := func(c conformer, orig, restored []byte) bool {
		eq := c.equal
		if eq == nil {
			eq = bytes.Equal
		}
		return eq(orig, restored)
	}

	logLines := []byte("ERROR conn refused 10.0.0.1:5432\nERROR conn refused 10.0.0.1:5432\n")

	// 1. NON-JSON CONTENT ROUND-TRIPS. This is the case that used to abort the suite.
	if !compare(conformer{name: "log"}, logLines, logLines) {
		t.Error("a lossless reducer over log lines cannot demonstrate its round-trip — the harness " +
			"still only admits JSON, which is what this fix was for")
	}

	// 2. AND THE DEFAULT IS STRICT. A restored form that differs by one byte must be rejected;
	// otherwise widening the harness would have bought coverage by lowering the bar.
	corrupted := []byte("ERROR conn refused 10.0.0.1:5432\nERROR conn refused 10.0.0.1:5433\n")
	if compare(conformer{name: "log"}, logLines, corrupted) {
		t.Error("the default comparison accepted a restored form that differs — byte-equality is " +
			"the point of the default and a lossless claim would mean nothing without it")
	}

	// 3. THE JSON ESCAPE HATCH IS REAL BUT NARROW: it accepts a re-ordered document (which is not a
	// content change) and still rejects a changed value (which is).
	jc := conformer{name: "json", equal: jsonSemanticEqual}
	if !compare(jc, []byte(`{"a":1,"b":2}`), []byte(`{"b":2,"a":1}`)) {
		t.Error("jsonSemanticEqual rejected a re-ordered document; key order is not content and " +
			"ExpandJSON re-encodes rather than replaying bytes")
	}
	if compare(jc, []byte(`{"a":1}`), []byte(`{"a":2}`)) {
		t.Error("jsonSemanticEqual accepted a CHANGED VALUE — the looser comparator would then be " +
			"a hole rather than an allowance")
	}
	if compare(jc, []byte(`{"a":1}`), []byte(`not json at all`)) {
		t.Error("jsonSemanticEqual reported equality against something it could not parse; a " +
			"comparator that cannot read its inputs must refuse, not pass")
	}
}

// ── the census reads DECLARATIONS, not spellings ─────────────────────────────────────────────────

// ⚠ THE CENSUS ABOVE IS THE ONLY THING THAT MAKES EVERY OTHER PROPERTY IN THIS FILE NON-OPT-IN, SO
// ITS OWN BLINDNESS IS THE WHOLE SUITE'S BLINDNESS. Measured 2026-08-28 (tab-q6d3) by dropping one
// new production file per arm into this package, each declaring a type the COMPILER certifies
// implements tare.Reduction (`var _ Reduction = ...`) and registered nowhere. Every arm below
// compiles and is gofmt-clean.
//
// The scan used to be a regex, `^func \(\w+ \*?(\w+)\) Reduce\(`, and it was wrong in BOTH
// directions:
//
//	BLIND — a real, unregistered reducer the census could not see, held to NO property at all:
//	  func (*T) Reduce(...)      unnamed pointer receiver  -> MISSED
//	  func (T) Reduce(...)       unnamed value receiver    -> MISSED
//	  func (t *T[P]) Reduce(...) generic receiver          -> MISSED
//	PHANTOM — a reducer that does not exist, which the census demanded be registered, so it FAILED
//	ON CORRECT CODE (the direction that gets guards deleted):
//	  the same line inside a /* block comment */          -> reported type "GhostComment"
//	  the same line inside a `raw string`                 -> reported type "GhostString"
//
// A named-receiver reducer WAS caught, which is why the regex looked like it worked: all four
// Reduce methods in this package happen to name their receiver, and an unnamed one is ordinary Go
// for a receiver a method does not use.
//
// A census by regex is a census of SPELLINGS. This drives the real scanner over synthetic sources
// so it cannot narrow back to one — every row below reds against the regex it replaced.
//
// ⚠ ONE THING IS STILL INVISIBLE TO THE CENSUS, AND IT IS NAMED HERE RATHER THAN LEFT TO BE
// REDISCOVERED — this file's own PrefixStable exclusion is the reason: an exclusion that goes
// unwritten is a promise nobody can check. `type W struct{ *JSONReducer }` satisfies Reduction by
// PROMOTION and declares no Reduce of its own, so it has no entry here and no row below. That is
// correct as long as it stays pure delegation: such a type has no behaviour that is not already
// held to the registry through the reducer it embeds, and the moment it declares its own Reduce to
// change any of that behaviour, the scan sees it. Measured, not assumed — the arm was run.
func TestReducerCensusReadsDeclarationsNotSpellings(t *testing.T) {
	const sig = "(_ context.Context, content []byte, kind Kind) ([]byte, int, int, error) { return content, 0, 0, nil }"

	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"named pointer receiver", "package tare\nfunc (r *Alpha) Reduce" + sig + "\n", []string{"Alpha"}},
		{"named value receiver", "package tare\nfunc (r Bravo) Reduce" + sig + "\n", []string{"Bravo"}},
		{"UNNAMED pointer receiver", "package tare\nfunc (*Charlie) Reduce" + sig + "\n", []string{"Charlie"}},
		{"UNNAMED value receiver", "package tare\nfunc (Delta) Reduce" + sig + "\n", []string{"Delta"}},
		{"generic receiver, one parameter", "package tare\nfunc (r *Echo[T]) Reduce" + sig + "\n", []string{"Echo"}},
		{"generic receiver, two parameters", "package tare\nfunc (r *Foxtrot[K, V]) Reduce" + sig + "\n", []string{"Foxtrot"}},
		{"two reducers in one file are both reported",
			"package tare\n\nfunc (r *Golf) Reduce" + sig + "\n\nfunc (r *Hotel) Reduce" + sig + "\n",
			[]string{"Golf", "Hotel"}},

		// ⚠ THE INVERTED ARMS. A census that reds on correct code gets relaxed until it reds on
		// nothing, so these matter as much as the blind ones.
		{"a block comment is documentation, not a reducer",
			"package tare\n\n/*\nfunc (g *GhostComment) Reduce" + sig + "\n*/\n", nil},
		{"a raw string is data, not a reducer",
			"package tare\n\nconst example = `\nfunc (g *GhostString) Reduce" + sig + "\n`\n", nil},
		{"a plain function named Reduce is not a method and cannot implement the interface",
			"package tare\nfunc Reduce" + sig + "\n", nil},
		{"a method with another name is not a reducer",
			"package tare\nfunc (r *India) Expand" + sig + "\n", nil},
	}

	for _, tc := range cases {
		got, err := reducerReceiversIn("probe.go", []byte(tc.src))
		if err != nil {
			t.Errorf("%s: scanner returned an error: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n  want %v\n  got  %v", tc.name, tc.want, got)
		}
	}
}

// ⚠ AND THE SCANNER MUST FAIL LOUDLY ON A FILE IT CANNOT READ. A scanner that returns nothing
// agrees with any registry — the same floor TestEveryReductionInThePackageIsRegistered's
// `scanned == 0` check exists for, one level down.
func TestReducerCensusRefusesSourceItCannotParse(t *testing.T) {
	if _, err := reducerReceiversIn("broken.go", []byte("package tare\nfunc (r *A) Reduce( {{{\n")); err == nil {
		t.Error("the scanner read unparseable source and reported no reducers; a silent zero from a " +
			"broken scan is indistinguishable from a correctly empty package")
	}
}
