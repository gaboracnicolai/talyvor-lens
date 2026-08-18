package econflags

// WHY THIS FILE EXISTS: THE GUARD THAT CLAIMED TO CATCH THIS DRIFT COULD NOT FAIL.
//
// econflags_test.go carried a test whose own docstring said "Named explicitly so a
// flag added to config without being added here fails the build's tests rather than
// silently going unobserved." MEASURED, NOT READ (harness
// ~/talyvor-queue/w61-econflags-controls.py, three controls, each restored in a
// finally and sha256-verified):
//
//	D1 a new flag added to config.go's `if !c.EconomyEnabled` block, not to econflags
//	   -> NOT CAUGHT, the whole package green. The test's want-list is a literal IN THE
//	   TEST; it reads nothing from config.go, so no change to config.go can red it. The
//	   sentence was a claim about a mechanism that did not exist.
//	D2 econflags.forceOff DROPS a flag config.go still force-offs -> NOT CAUGHT. The
//	   readout then answers "off" where the truth is "forced_off": the operator reads
//	   their env var as simply unset, when config.Load is overwriting it.
//	D3 econflags.forceOff GAINS a flag config.go does NOT force off -> NOT CAUGHT. The
//	   readout answers "forced_off" for a flag that IS honouring its environment — the
//	   operator is told their setting is not in effect when it is.
//
// D2 and D3 are the two halves of rule 2 in this package's own doc comment, and
// nothing could see either. The transcription in forceOff is a COPY of a block in
// another file, so it decays with no commit in this package at all — the same shape
// as a cross-repo line citation, one directory over.
//
// THE RULES BELOW DERIVE FROM config.go's OWN DECLARATION rather than inventing a
// definition of "economy flag": the force-off block is a boundary a human already
// drew, so rules A and B need no new taxonomy. Rule C does need one and says so.
//
// This file is in package econflags (not econflags_test) deliberately: rule A reads
// the unexported forceOff map, because a key in that map reached by no entry is dead
// transcription that an observation of Report() alone cannot see.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/config"
)

const configPath = "../config/config.go"

// FLOORS. Every rule below is a set comparison, and a set comparison over an EMPTY
// set passes. These are what stands between "the transcription agrees with config.go"
// and "my parser read nothing and reported a clean product". Measured at the commit
// that introduced this file: 16 force-off assignments, 60 bool fields, 15 of them
// Mint-or-LXC named. Set below today's numbers so an ordinary addition does not red
// them, but far enough above zero that a parse returning nothing does.
const (
	floorForceOffFlags = 16
	floorConfigBools   = 50
	floorMoneyNamed    = 12
)

// parseConfigForceOffBlock returns the flags config.go sets to false inside
// `if !c.EconomyEnabled { ... }` — the force-off block, read from the source that
// performs it rather than from a comment describing it.
func parseConfigForceOffBlock(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}

	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		// Cond must be exactly `!c.EconomyEnabled`.
		un, ok := ifs.Cond.(*ast.UnaryExpr)
		if !ok || un.Op != token.NOT {
			return true
		}
		sel, ok := un.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "EconomyEnabled" {
			return true
		}
		for _, stmt := range ifs.Body.List {
			as, ok := stmt.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				continue
			}
			lhs, ok := as.Lhs[0].(*ast.SelectorExpr)
			if !ok {
				continue
			}
			rhs, ok := as.Rhs[0].(*ast.Ident)
			if !ok || rhs.Name != "false" {
				continue
			}
			found[lhs.Sel.Name] = true
		}
		return true
	})
	return found
}

// parseConfigBoolFields returns every bool field declared on config.Config.
func parseConfigBoolFields(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Config" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, fld := range st.Fields.List {
			id, ok := fld.Type.(*ast.Ident)
			if !ok || id.Name != "bool" {
				continue
			}
			for _, name := range fld.Names {
				out = append(out, name.Name)
			}
		}
		return false
	})
	sort.Strings(out)
	return out
}

func reportedNames(t *testing.T) map[string]bool {
	t.Helper()
	// EconomyEnabled=true so no flag is suppressed by a force-off state; the entry
	// list does not depend on the values, but reading it under the ON case keeps this
	// helper honest if that ever changes.
	snap := Report(&config.Config{EconomyEnabled: true}, "abc1234")
	if !snap.Observed {
		t.Fatal("a live config must be observed")
	}
	out := map[string]bool{}
	for _, f := range snap.Flags {
		out[f.Name] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RULE A — the transcription must EQUAL config.go's force-off block, both directions.
//
// This is the rule D2 and D3 walked past. A missing key makes the readout answer
// "off" where the truth is "forced_off"; an extra key makes it answer "forced_off"
// about a flag that is honouring its environment. Both are wrong in the one way this
// package exists to prevent, and neither is visible from the value.
func TestForceOffTranscriptionEqualsConfig(t *testing.T) {
	inConfig := parseConfigForceOffBlock(t)
	if len(inConfig) < floorForceOffFlags {
		t.Fatalf("parsed only %d flags out of %s's force-off block (floor %d) — the parser read "+
			"nothing or read the wrong block, and every comparison below would pass vacuously",
			len(inConfig), configPath, floorForceOffFlags)
	}

	for name := range inConfig {
		if !forceOff[name] {
			t.Errorf("config.go force-offs %q and econflags.forceOff does not list it — the "+
				"readout will report it as a plain \"off\", so an operator whose env var is "+
				"being overwritten is told it simply is not set", name)
		}
	}
	for name := range forceOff {
		if !inConfig[name] {
			t.Errorf("econflags.forceOff lists %q and config.go does NOT force it off — the "+
				"readout will report \"forced_off\" for a flag that IS honouring its "+
				"environment, telling an operator their setting is not in effect when it is", name)
		}
	}
}

// RULE B — every flag config.go force-offs must actually be REPORTED.
//
// This is the sentence the old test claimed and could not keep, restated against the
// source instead of against a literal in the test. A flag in the force-off block that
// no entry reads is a money-path flag the readout is silent about, and D1 proved the
// silence was reachable by an ordinary edit to config.go.
func TestEveryForceOffFlagIsReported(t *testing.T) {
	inConfig := parseConfigForceOffBlock(t)
	if len(inConfig) < floorForceOffFlags {
		t.Fatalf("parsed only %d force-off flags (floor %d) — vacuous comparison refused",
			len(inConfig), floorForceOffFlags)
	}
	reported := reportedNames(t)
	for _, name := range sortedKeys(inConfig) {
		if !reported[name] {
			t.Errorf("config.go force-offs %q and econflags does not report it at all — it is a "+
				"money-path flag and the readout is silent about it", name)
		}
	}
}

// RULE C — every Mint-named or LXC-named config bool must be reported.
//
// ⚠ THIS IS A NAME-SHAPE RULE AND THEREFORE A FLOOR, NOT A CENSUS. Rules A and B lean
// on a boundary config.go already draws; there is no such declaration for "money-path
// flag", so this one is keyed on the name — the same weak instrument that let
// formatterReach watch a defect happen one repo over. It is here because it is
// CHECKABLE and it caught four live omissions, not because it is complete.
//
// ⚠ WHAT IT CANNOT SEE, MEASURED AT THE COMMIT THAT ADDED IT: KeelRoyaltyHaircutEnabled
// is a DEFAULT-ON reduce-only haircut on reuse-royalty mints and matches neither
// "Mint" nor "LXC", so it is outside this rule and outside the readout. It is recorded
// here rather than quietly added, because widening the readout to it needs a rule for
// what a money-path flag IS, and that rule is the work — not a name I happened to
// notice. Same for NodeAutoRouteEnabled (routing, mints downstream).
func TestEveryMintOrLXCNamedFlagIsReported(t *testing.T) {
	bools := parseConfigBoolFields(t)
	if len(bools) < floorConfigBools {
		t.Fatalf("parsed only %d bool fields on config.Config (floor %d) — the struct walk read "+
			"nothing and the comparison below would pass vacuously", len(bools), floorConfigBools)
	}

	var moneyNamed []string
	for _, name := range bools {
		if strings.Contains(name, "Mint") || strings.Contains(name, "LXC") {
			moneyNamed = append(moneyNamed, name)
		}
	}
	if len(moneyNamed) < floorMoneyNamed {
		t.Fatalf("only %d Mint-or-LXC named bools found (floor %d) — the name filter matched "+
			"nothing and this rule would pass over any omission", len(moneyNamed), floorMoneyNamed)
	}

	reported := reportedNames(t)
	for _, name := range moneyNamed {
		if !reported[name] {
			t.Errorf("config.Config.%s is a Mint-or-LXC named flag and econflags does not report "+
				"it — this package's doc comment promises the economy and minting flags, and a "+
				"readout that omits one looks exactly like a readout that had none to show", name)
		}
	}
}

// RULE E — the reported set is PINNED, so a REMOVAL is a diff a reviewer sees.
//
// Rules A/B/C are floors: they all say "this must be present". None of them can see an
// entry being DELETED unless config.go happens to require it, and the four flags added
// alongside this file are required by rule C's name shape alone — a rule this file
// itself calls weak. Exact equality is the companion that makes any change to the
// money readout, in either direction, appear in a diff.
//
// If this test fails because a flag was legitimately added, add it here in the same
// commit. That is the point: the pin is not an obstacle, it is the review.
func TestReportedFlagSetIsPinned(t *testing.T) {
	want := []string{
		"AdminLXCGrantEnabled",
		"AnnotationMintingEnabled",
		"CachePoolableEnabled",
		"CacheSharingEnabled",
		"ConfidentialMintingEnabled",
		"DistillPoolableEnabled",
		"EconomyEnabled",
		"EvalContributionMintingEnabled",
		"LXCAgentAllocationEnabled",
		"LXCGatingEnabled",
		"LXCReservationEnabled",
		"LXCShadowSpendEnabled",
		"LatencyMintingEnabled",
		"POVIMintingEnabled",
		"PatternCaptureEnabled",
		"PatternEarningEnabled",
		"PatternMiningEnabled",
		"PoolRoyaltyMintingEnabled",
		"ProofOfImprovementEnabled",
		"ReputationBondedMintingEnabled",
		"RoutingIntelligenceEnabled",
		"RoutingPredictionMintingEnabled",
		"RoutingTierCohortsEnabled",
		"ShadowMintsEnabled",
		"TrustfulComputeMintEnabled",
	}
	reported := reportedNames(t)

	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, w := range want {
		if !reported[w] {
			t.Errorf("pinned flag %q is no longer reported — the money readout lost a flag and "+
				"nothing else in this package would have said so", w)
		}
	}
	for _, got := range sortedKeys(reported) {
		if !wantSet[got] {
			t.Errorf("flag %q is reported and not pinned — add it to this list in the same commit "+
				"that adds it to the readout, so the change to a money surface is reviewable",
				got)
		}
	}
	if len(reported) != len(want) {
		t.Errorf("readout reports %d flags, pin holds %d", len(reported), len(want))
	}
}

// Env strings are part of the readout's usefulness: a flag reported without the
// variable that sets it cannot be acted on. Pinned against config.go's own reads so a
// copy-paste that names the wrong variable reds here rather than sending an operator
// to edit a variable the process never looks at.
func TestReportedEnvNamesMatchConfig(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, configPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}
	// Collect every string literal argument to parseBoolEnv(...) in config.go.
	envs := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "parseBoolEnv" || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		envs[strings.Trim(lit.Value, `"`)] = true
		return true
	})
	if len(envs) < floorConfigBools {
		t.Fatalf("found only %d parseBoolEnv call sites in %s (floor %d) — the scan read nothing",
			len(envs), configPath, floorConfigBools)
	}

	snap := Report(&config.Config{EconomyEnabled: true}, "abc1234")
	for _, fl := range snap.Flags {
		if fl.Env == "" {
			t.Errorf("flag %q reports no env var", fl.Name)
			continue
		}
		if !envs[fl.Env] {
			t.Errorf("flag %q reports env %q and config.go never reads that variable — an "+
				"operator setting it would change nothing", fl.Name, fl.Env)
		}
	}
}
