package proxy

import (
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/poolshadow"
)

// fakePoolShadowSink records what the proxy hands it. It is deliberately the ONLY thing this test
// gives the proxy — no database — so a green here cannot come from a database that happened to work.
type fakePoolShadowSink struct {
	mu   sync.Mutex
	got  []poolshadow.Observation
	fail error
}

func (f *fakePoolShadowSink) Record(_ context.Context, o poolshadow.Observation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, o)
	return f.fail
}

func (f *fakePoolShadowSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

// TestShadowPool_InertByDefault — and TWO-SIDED, which is the half that matters. A "nothing was
// recorded" assertion is worthless unless the same harness can be shown recording something.
func TestShadowPool_InertByDefault(t *testing.T) {
	sink := &fakePoolShadowSink{}
	p := &Proxy{}

	// (1) Nothing wired at all.
	p.shadowPoolObservation(context.Background(), "wsA", "openai", "gpt-4o", "hello", false)
	if n := sink.count(); n != 0 {
		t.Fatalf("unwired proxy recorded %d observations, want 0", n)
	}

	// (2) Sink wired, flag OFF.
	p.SetPoolShadowLog(sink, func() bool { return false })
	p.shadowPoolObservation(context.Background(), "wsA", "openai", "gpt-4o", "hello", false)
	if n := sink.count(); n != 0 {
		t.Fatalf("flag off recorded %d observations, want 0", n)
	}

	// (3) THE CONTROL: flag ON must record, or (1) and (2) prove only that this harness is inert.
	p.SetPoolShadowLog(sink, func() bool { return true })
	p.shadowPoolObservation(context.Background(), "wsA", "openai", "gpt-4o", "hello", false)
	if n := sink.count(); n != 1 {
		t.Fatalf("CONTROL FAILED: flag on recorded %d observations, want 1 — the two zeros above "+
			"were this test being unable to observe anything, not the feature being inert", n)
	}
}

// TestShadowPool_RecordsTheProductionPooledKeyAndCarriesNoPromptText.
//
// Two assertions in one place because they are the same claim from both sides: what goes in the row
// is the key material production pools on, and what does NOT go in is the prompt.
func TestShadowPool_RecordsTheProductionPooledKeyAndCarriesNoPromptText(t *testing.T) {
	sink := &fakePoolShadowSink{}
	p := &Proxy{}
	p.SetPoolShadowLog(sink, func() bool { return true })

	const raw = "how do I fix ImportError in python 3.12"
	p.shadowPoolObservation(context.Background(), "wsA", "openai", "gpt-4o", raw, false)
	if sink.count() != 1 {
		t.Fatalf("recorded %d, want 1", sink.count())
	}
	got := sink.got[0]

	// The fingerprint must equal one built from the PRODUCTION pooled key rule. If the proxy ever
	// stops feeding pooledPromptKey (or the rule changes on one side only), the shadow numbers
	// silently stop describing the pool — and nothing else in the tree would notice.
	want := poolshadow.Observe("wsA", "openai", "gpt-4o", pooledPromptKey(raw), raw, false)
	if string(got.PooledKeyFP) != string(want.PooledKeyFP) {
		t.Fatal("the recorded fingerprint is not the one built from pooledPromptKey — the shadow log " +
			"is measuring a keyspace the pool does not use")
	}
	if len(got.PooledKeyFP) != 32 {
		t.Fatalf("fingerprint is %d bytes, want 32 (sha256)", len(got.PooledKeyFP))
	}

	// No prompt text on the row, in any field. A shadow log that carries prompts is a prompt log.
	for name, v := range map[string]string{
		"WorkspaceID": got.WorkspaceID, "Provider": got.Provider, "Model": got.Model,
		"PooledKeyFP": string(got.PooledKeyFP), "CanonFP": string(got.CanonFP),
	} {
		if strings.Contains(v, "ImportError") || strings.Contains(v, "python") {
			t.Fatalf("field %s carries prompt text: %q", name, v)
		}
	}
	// CONTROL for the sweep above: it must be able to SEE prompt text when prompt text is there.
	if !strings.Contains(pooledPromptKey(raw), "ImportError") {
		t.Fatal("CONTROL FAILED: pooledPromptKey does not contain the prompt, so the no-prompt-text " +
			"sweep above proves nothing about whether it could have found any")
	}
}

// TestShadowPool_SinkErrorsCannotAffectTheServePath — void by construction, asserted on the
// SIGNATURE rather than on behaviour, because a signature cannot regress quietly.
func TestShadowPool_SinkErrorsCannotAffectTheServePath(t *testing.T) {
	sink := &fakePoolShadowSink{fail: context.DeadlineExceeded}
	p := &Proxy{}
	p.SetPoolShadowLog(sink, func() bool { return true })
	p.shadowPoolObservation(context.Background(), "wsA", "openai", "gpt-4o", "hello", false)

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "shadow_pool.go", nil, 0)
	if err != nil {
		t.Fatalf("parse shadow_pool.go: %v", err)
	}
	var found bool
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "shadowPoolObservation" {
			continue
		}
		found = true
		if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
			t.Fatal("shadowPoolObservation now returns a value — a serve path could branch on it, " +
				"and the safety argument in this file's header is that it cannot")
		}
	}
	if !found {
		t.Fatal("shadowPoolObservation is gone — re-anchor this guard")
	}
}

// TestShadowPool_DoesNotReadThePooledKeyspace — the guard that keeps this file from being
// "improved" into the structurally-zero lookup design.
//
// ⚠ THIS IS THE GUARD THAT ENCODES THE FINDING. The natural "improvement" to this file is to do the
// pooled LOOKUP on a private miss and record the would-be hit. That design reports ZERO forever,
// because the pooled keyspace is only written under the same PoolabilityGate.Participant predicate
// that gates reading it — turn pooling off and the thing being measured is emptied with it. The
// resulting number is structurally zero and reads as measured. So the read surfaces are named here
// and forbidden in this file.
func TestShadowPool_DoesNotReadThePooledKeyspace(t *testing.T) {
	src, err := os.ReadFile("shadow_pool.go")
	if err != nil {
		t.Fatalf("read shadow_pool.go: %v", err)
	}
	body := string(src)
	// Strip comments: the header EXPLAINS these surfaces, and a census that cannot tell an
	// explanation from a call would fire on the documentation of its own reason for existing.
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, "shadow_pool.go", nil, 0)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	var calls []string
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				calls = append(calls, sel.Sel.Name)
			} else if id, ok := c.Fun.(*ast.Ident); ok {
				calls = append(calls, id.Name)
			}
		}
		return true
	})
	forbidden := []string{"tryExactPooled", "trySemanticPooled", "GetPooled", "GetWithOwner", "SetWithOwner", "SetPooled"}
	for _, name := range calls {
		for _, bad := range forbidden {
			if name == bad {
				t.Fatalf("shadow_pool.go calls %s — that is the LOOKUP design, which can only ever "+
					"report zero while pooling is off. Read this file's header before changing it.", bad)
			}
		}
	}
	// A floor: if the walk found no calls at all it would pass over an empty set.
	if len(calls) < 5 {
		t.Fatalf("the call walk found %d calls, expected at least 5 — it has gone blind and its "+
			"verdict above is vacuous", len(calls))
	}
	// CONTROL: the forbidden names must be findable by this walk's shape when they ARE present.
	// pooledPromptKey is the production key call this file legitimately makes; if the walk cannot
	// see that, it cannot see anything.
	var sawKeyCall bool
	for _, name := range calls {
		if name == "pooledPromptKey" {
			sawKeyCall = true
		}
	}
	if !sawKeyCall {
		t.Fatal("CONTROL FAILED: the walk did not find the pooledPromptKey call this file certainly " +
			"makes, so its 'no forbidden call' verdict is a statement about the walk, not the file")
	}
	if !strings.Contains(body, "tryExactPooled") {
		t.Fatal("the header no longer names the lookup design it exists to rule out — that prose is " +
			"the only place the reason is written down")
	}
}

// TestShadowPool_SinkSurfaceIsPersistOnly — the interface cannot serve, look up, or credit.
func TestShadowPool_SinkSurfaceIsPersistOnly(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "shadow_pool.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var methods []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "poolShadowSink" {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range it.Methods.List {
			for _, nm := range m.Names {
				methods = append(methods, nm.Name)
			}
		}
		return false
	})
	if len(methods) != 1 || methods[0] != "Record" {
		t.Fatalf("poolShadowSink exposes %v, want exactly [Record] — a second method is a way for "+
			"the serve path to reach something that is not persistence", methods)
	}
}

// TestShadowPool_CallSiteGatesOnShouldCache — the denominator guard.
//
// A response the product would not cache could never have been pooled, so logging it would put
// rows in the denominator that no amount of pooling could convert: a rate deflated by construction.
//
// ⚠ THIS GUARD WAS FIRST WRITTEN AS A 20-LINE TEXT WINDOW ABOVE THE CALL SITE AND IT COULD NOT
// FAIL. Deleting the gate left the mutation UNCAUGHT, because the window still found the word
// `shouldCache` — in the ENCLOSING `if shouldCache {` block that guards storeCaches, fifteen lines
// up. The guard was reading a different gate on a different call and reporting it as this one's.
// Measured by control C9 in ~/talyvor-queue/w49-poolshadow-controls-z8k5.py. It now resolves the
// call's actual enclosing `if` condition through the AST, so only THIS call's own gate can satisfy it.
func TestShadowPool_CallSiteGatesOnShouldCache(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}

	// Walk with a stack so each call knows the `if` statements it is lexically inside.
	type frame struct{ cond string }
	var stack []frame
	var sites, gated int
	var ungatedAt []int

	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.IfStmt:
			var b strings.Builder
			if err := printer.Fprint(&b, fset, node.Cond); err != nil {
				t.Fatalf("print cond: %v", err)
			}
			stack = append(stack, frame{cond: b.String()})
			ast.Inspect(node.Body, visit)
			stack = stack[:len(stack)-1]
			if node.Else != nil {
				ast.Inspect(node.Else, visit)
			}
			return false
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "shadowPoolObservation" {
				return true
			}
			sites++
			// The IMMEDIATELY enclosing condition, not any ancestor: an outer block's gate is a
			// different gate on a different call.
			if len(stack) > 0 && strings.Contains(stack[len(stack)-1].cond, "shouldCache") {
				gated++
			} else {
				ungatedAt = append(ungatedAt, fset.Position(node.Pos()).Line)
			}
			return true
		}
		return true
	}
	ast.Inspect(f, visit)

	if sites == 0 {
		t.Fatal("no shadowPoolObservation call site in proxy.go — the log is wired to nothing and " +
			"every figure it produces is zero")
	}
	if len(ungatedAt) > 0 {
		t.Fatalf("%d of %d shadowPoolObservation call sites are not gated on shouldCache by their "+
			"own enclosing condition (proxy.go lines %v) — uncacheable responses would enter the "+
			"denominator and deflate the rate", len(ungatedAt), sites, ungatedAt)
	}

	// CONTROL: the walk must be able to report an UNGATED call when there is one. Without it,
	// "0 ungated" is indistinguishable from a walk that never resolves a condition at all.
	if gated != sites {
		t.Fatalf("accounting is inconsistent: %d gated of %d sites with no ungated list", gated, sites)
	}
	if len(stack) != 0 {
		t.Fatalf("the if-stack did not unwind (%d frames left) — the walk is not tracking scope, "+
			"so its gating verdict is not about scope either", len(stack))
	}
}

// TestShadowPool_CoveragePopulationIsExplicit — the population census.
//
// ⚠ THE DEFECT THIS EXISTS TO PREVENT IS A RATE THAT DESCRIBES LESS TRAFFIC THAN IT APPEARS TO.
// Every response that reaches storeCaches is a candidate the pool could have held; a serve lane
// that writes the cache but not the shadow log is invisible to the rate while still contributing
// to the real pool. Nothing about the resulting number would look partial.
//
// So the census pairs the two by ENCLOSING FUNCTION and requires every unpaired lane to be named
// here, with its reason, rather than merely being absent.
func TestShadowPool_CoveragePopulationIsExplicit(t *testing.T) {
	// Lanes that write the cache but deliberately do NOT log a shadow observation.
	//
	// ⚠ AND THE EXEMPTION HAS A COST THAT IS RECORDED HERE RATHER THAN DISCOVERED LATER. Both are
	// served WITHOUT a paid cloud-provider call, so a pooled hit on one avoids no provider spend —
	// which is the saving the rate is meant to price. But storeCaches DOES write their responses
	// into the pooled keyspace, so in production a cloud request could be served from a local or
	// node contribution. The shadow rate therefore under-counts by exactly those hits. It is a
	// FLOOR on both counts (this and LoggingNone, see the proxy.go call site).
	//
	// ⚠ KEYED ON file+FUNCTION, NOT ON THE FUNCTION NAME. The first cut of this census keyed on the
	// name alone and COULD NOT FAIL for the streaming lane: proxy.go's buffered serve and
	// stream.go's streaming serve are BOTH called `serve`, so the buffered lane's coverage vouched
	// for the streaming one. Deleting the streaming hook outright left the census GREEN — measured
	// as control C13 in ~/talyvor-queue/w49-poolshadow-controls-z8k5.py. A collision in a census key
	// reports coverage it never checked, and the two colliding lanes here are precisely the two that
	// carry a paid provider call.
	exempt := map[string]string{
		"proxy.go:tryNodeRouting":  "served by a network node, not a paid cloud provider",
		"proxy.go:tryLocalRouting": "served by a local model, not a paid cloud provider",
	}

	type site struct{ fn, file string }
	var stores, shadows []site

	for _, file := range []string{"proxy.go", "stream.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "storeCaches":
					stores = append(stores, site{fn.Name.Name, file})
				case "shadowPoolObservation":
					shadows = append(shadows, site{fn.Name.Name, file})
				}
				return true
			})
		}
	}

	// Floors on both populations. A census over an empty set asserts nothing, and this one has TWO
	// sets — either going blind independently would produce a confident, meaningless verdict.
	if len(stores) < 4 {
		t.Fatalf("found %d storeCaches call sites, expected at least 4 — the walk has gone blind", len(stores))
	}
	if len(shadows) == 0 {
		t.Fatal("found NO shadowPoolObservation call site — the shadow log is wired to nothing")
	}

	key := func(s site) string { return s.file + ":" + s.fn }
	covered := map[string]bool{}
	for _, s := range shadows {
		covered[key(s)] = true
	}
	for _, s := range stores {
		if covered[key(s)] {
			continue
		}
		if _, ok := exempt[key(s)]; !ok {
			t.Fatalf("%s (%s) writes the cache but logs no shadow observation, and is not in the "+
				"exempt list — its traffic would be missing from the denominator while still "+
				"contributing to the real pool. Cover it, or name it here with the reason.", s.fn, s.file)
		}
	}
	// The exempt list must not outlive the lanes it names: a stale entry silently re-permits a gap.
	seen := map[string]bool{}
	for _, s := range stores {
		seen[key(s)] = true
	}
	for fn := range exempt {
		if !seen[fn] {
			t.Fatalf("exempt lane %q no longer calls storeCaches — remove it, or the next lane that "+
				"takes that name inherits an exemption nobody granted it", fn)
		}
	}

	// The two lanes that carry a PAID provider call must BOTH be covered, named individually. The
	// loop above only proves "no uncovered, unexempt lane" — a census can satisfy that by finding
	// nothing at all. This says which lanes must be there.
	for _, want := range []string{"proxy.go:serve", "stream.go:serve"} {
		if !covered[want] {
			t.Fatalf("%s has no shadow observation — it is a paid-provider serve lane, so its "+
				"traffic belongs in the denominator", want)
		}
	}
}
