package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// wiring_reachability_scan_test.go — the structural reader for "is this SAFETY hook wired
// unconditionally at boot".
//
// ⚠ WHY THIS EXISTS. Three guards assert the same invariant — a safety restriction must NOT
// be liftable by the economy master kill: the U6 Sybil mint floor (SetMintVerifier), the PR2
// wash hardening (SetMintRateCap, SetOwnerLinkageCheck) and the U18 fiat LXC wiring
// (SetLXCGate, SetLXCSpendSink). All three answered it with
//
//	strings.HasPrefix(line, "\t"+hook)   // "exactly one leading tab"
//
// which asks whether a LINE OF TEXT looks like a top-level statement, not whether a CALL runs
// unconditionally. Measured against the real guards by rewriting the SetMintVerifier wiring in
// main.go (~/talyvor-queue/w61-unconditional-wiring-controls-h2r7.py):
//
//	the call deleted                                   → CAUGHT (positive control)
//	the call moved inside `if cfg.EconomyEnabled`      → CAUGHT (positive control 2)
//	moved into a helper called `if cfg.EconomyEnabled` → MISSED
//	moved into a helper that is NEVER CALLED           → MISSED
//	deleted, the call text left in a RAW STRING whose
//	  content line begins with one tab                 → MISSED
//
// The third is the invariant exactly inverted: the economy master kill lifts the Sybil mint
// floor and the guard reports it unconditional. The fourth and fifth mean the floor never
// enforces at all. A helper body is indented with ONE TAB like any function body, so the tab
// proxy cannot tell run()'s straight-line boot from any other function's — and a raw string
// costs nothing at all.
//
// ⚠ THE SPLIT: the STRUCTURAL question (does this call run unconditionally when run() runs)
// goes to go/parser; the questions genuinely ABOUT NAMES (which hook, which entry point) stay
// strings. Comments and string literals are not in the AST.

// bootEntryPoint is the function whose straight-line statements are the boot path.
const bootEntryPoint = "run"

// wiringSite is one call to a wiring hook found in main.go. The hook may be a METHOD
// (`tokenLedger.SetMintVerifier`) or a plain FUNCTION (`mountSessionKeyRoutes`); receiver is
// empty for the latter.
type wiringSite struct {
	method   string
	receiver string
	fn       string // the top-level function containing it
	guarded  bool   // enclosed by if/for/switch/select, or inside a function literal
	line     int
	// conds are the `if` conditions that really enclose the call, outermost first. `guarded`
	// says THAT something governs it; conds says WHAT, which is the question a guard asking
	// "is this inside `if sessionKeyStore != nil`" actually means.
	conds []string
	// args are the rendered arguments, so a guard can ask what the call was given.
	args []string
}

// condsInclude reports whether cond is among the call's enclosing conditions.
func (s wiringSite) condsInclude(cond string) bool {
	for _, c := range s.conds {
		if c == cond {
			return true
		}
	}
	return false
}

// wiringScan is one parse of main.go.
type wiringScan struct {
	sites []wiringSite
	// reached is every top-level function unconditionally reached from bootEntryPoint.
	reached map[string]bool
}

// unconditional reports whether receiver.method has a call site that runs whenever run() runs:
// not enclosed by any conditional, in a function unconditionally reached from run().
//
// ⚠ THE RECEIVER IS PART OF THE QUESTION and is kept so these assertions stay exactly the ones
// they replace: main.go calls SetOwnerLinkageCheck on BOTH royaltyMinter and distillMinter, and
// the guard that pins it names royaltyMinter. Matching on the method alone would let either
// site satisfy the other's assertion.
func (w *wiringScan) unconditional(receiver, method string) bool {
	for _, s := range w.sites {
		if s.receiver == receiver && s.method == method && !s.guarded && w.reached[s.fn] {
			return true
		}
	}
	return false
}

// present reports whether receiver.method is called anywhere at all — the weaker half, kept so
// "never wired" and "wired but liftable" stay distinguishable in the error messages.
func (w *wiringScan) present(receiver, method string) bool {
	for _, s := range w.sites {
		if s.receiver == receiver && s.method == method {
			return true
		}
	}
	return false
}

// scanWiring parses src and resolves, for the given hook method names, every call site and
// which functions run() reaches unconditionally. Parse errors are RETURNED: a scan that finds
// no call sites reports every hook as missing, and one that resolves no reachability reports
// every hook as conditional — both are wrong in a direction that hides the real state.
func scanWiring(filename string, src []byte, methods map[string]bool) (*wiringScan, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	w := &wiringScan{reached: map[string]bool{}}
	// edges[caller] = callees it reaches with no conditional in between.
	edges := map[string][]string{}

	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fname := fd.Name.Name
		var stack []ast.Node
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			guarded := conditionallyReached(stack)
			args := make([]string, 0, len(call.Args))
			for _, a := range call.Args {
				args = append(args, types.ExprString(a))
			}
			site := wiringSite{
				fn:      fname,
				guarded: guarded,
				line:    fset.Position(call.Pos()).Line,
				conds:   enclosingIfConds(stack),
				args:    args,
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && methods[sel.Sel.Name] {
				site.method, site.receiver = sel.Sel.Name, types.ExprString(sel.X)
				w.sites = append(w.sites, site)
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				// A plain FUNCTION hook — mountSessionKeyRoutes, applyCatalogOverrides — is a
				// wiring call too, and the guards over those asked about it in raw text.
				if methods[id.Name] {
					site.method = id.Name
					w.sites = append(w.sites, site)
				}
				if !guarded {
					edges[fname] = append(edges[fname], id.Name)
				}
			}
			return true
		})
	}

	w.reached[bootEntryPoint] = true
	for grew := true; grew; {
		grew = false
		for caller, callees := range edges {
			if !w.reached[caller] {
				continue
			}
			for _, c := range callees {
				if !w.reached[c] {
					w.reached[c] = true
					grew = true
				}
			}
		}
	}
	return w, nil
}

// conditionallyReached reports whether anything between the enclosing function body and the
// node makes the node's execution conditional or deferred. A function LITERAL counts: a
// closure body is not a straight-line boot statement even when it is written at one tab.
func conditionallyReached(stack []ast.Node) bool {
	for i := 0; i+1 < len(stack); i++ {
		switch x := stack[i].(type) {
		case *ast.FuncLit:
			return true
		case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			return true
		case *ast.IfStmt:
			// A call in the if's own CONDITION always evaluates; one in a branch does not.
			if child := stack[i+1]; ast.Node(x.Body) == child || (x.Else != nil && x.Else == child) {
				return true
			}
		}
	}
	return false
}

// assignConds returns, for every assignment to ident in filename, the `if` conditions that
// really enclose it. It exists because "the store is only constructed when the config says so"
// is a question about WHERE the assignment sits, and the guard that asked it was satisfied by
// the flag's `if` appearing anywhere in the file.
func assignConds(filename string, src []byte, ident string) (conds [][]string, found bool, err error) {
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, filename, src, 0)
	if perr != nil {
		return nil, false, perr
	}
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, l := range as.Lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == ident {
				found = true
				conds = append(conds, enclosingIfConds(stack))
			}
		}
		return true
	})
	return conds, found, nil
}

// callOffsets returns the byte offset of every real CALL in filename whose function renders to
// name. Offsets rather than lines so a guard with a deliberate BYTE window — provisioning's
// warn-adjacency rule — keeps its window exactly and only stops counting comments as calls.
func callOffsets(filename string, src []byte, name string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []int
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && types.ExprString(c.Fun) == name {
			out = append(out, fset.Position(c.Pos()).Offset)
		}
		return true
	})
	return out, nil
}

// stringLiteralOffsets returns the byte offset of every STRING LITERAL in filename whose value
// contains substr. A comment mentioning the same text is not a string literal, which is the
// whole difference.
func stringLiteralOffsets(filename string, src []byte, substr string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []int
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, uerr := strconv.Unquote(lit.Value); uerr == nil && strings.Contains(v, substr) {
			out = append(out, fset.Position(lit.Pos()).Offset)
		}
		return true
	})
	return out, nil
}

// anyWithin reports whether any offset falls in [lo, hi).
func anyWithin(offsets []int, lo, hi int) bool {
	for _, o := range offsets {
		if o >= lo && o < hi {
			return true
		}
	}
	return false
}
