package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// batch_wiring_scan_test.go — the structural reader for the /v1/batch lane's wiring.
//
// ⚠ WHY THIS EXISTS. Four guards decide the batch lane's safety from main.go's TEXT, and the
// lane's own header says what a mistake costs: it bills nothing AND GET /v1/batch/jobs returns
// batchRouter.ListJobs() with no workspace filter, over BatchJob values carrying Prompt and
// Response. A re-opened bare route is a CROSS-TENANT READ, not merely an unbilled one.
// Measured against the real guards (~/talyvor-queue/w61-batchlane-controls-h2r7.py and
// w61-batchlaneopen-controls-h2r7.py), the whole cmd/lens package run per arm:
//
//	a bare authed.Post("/v1/batch/probe", h) on one line     → CAUGHT (positive control)
//	the same with authed.Put                                 → MISSED
//	the same with r.Handle — the UNAUTHENTICATED root router  → MISSED
//	the same split across lines                              → MISSED
//	the gated submit registration deleted                    → CAUGHT (positive control 2)
//	deleted, its exact text left behind in a COMMENT         → MISSED
//	the lane OPENED (settleWired true) while ListJobs is
//	  unscoped                                               → CAUGHT (positive control 3)
//	opened, the old closed text left in a COMMENT            → MISSED
//	opened, the old closed text quoted in a STRING           → MISSED
//	still CLOSED but written across lines                    → FALSE RED
//
// The last four are one guard, TestBatchLane_CannotOpenWhileTheJobListIsUnscoped, and it is
// wrong in BOTH directions: it returns early — enforcing nothing — when the closed TEXT appears
// anywhere, and it accuses correct code that is merely formatted differently.
//
// ⚠ THE SPLIT: the STRUCTURAL questions (is this a registration, does it go through the gate,
// what boolean was passed) go to go/parser; the questions genuinely ABOUT NAMES (which path,
// which gate) stay strings.

const (
	batchPathPrefix = "/v1/batch"
	batchGateVar    = "batchGate"
	batchRegFunc    = "newBatchReg"
)

// gatedBatchRoute is one batchGate.{get,post,del}(router, "path", handler) call.
type gatedBatchRoute struct {
	verb    string
	path    string
	handler string // the rendered handler argument
	line    int
}

// batchWiring is what one parse of main.go says about the lane.
type batchWiring struct {
	bare        []routeReg        // /v1/batch paths bound DIRECTLY on a router — the banned shape
	gated       []gatedBatchRoute // bound through the gate
	settleWired string            // the rendered second argument of newBatchReg(...), "" if absent
}

// scanBatchWiring parses src and answers all four of the lane's questions. Parse errors are
// RETURNED: a scan that finds no bare routes reports the lane safe, which is the wrong direction.
func scanBatchWiring(filename string, src []byte) (*batchWiring, error) {
	regs, _, _, err := scanRouteRegistrations(filename, src)
	if err != nil {
		return nil, err
	}
	w := &batchWiring{}
	for _, r := range regs {
		if strings.HasPrefix(r.path, batchPathPrefix) {
			w.bare = append(w.bare, r)
		}
	}

	// ⚠ ONE ERROR PATH, DELIBERATELY. scanRouteRegistrations above already parsed these exact
	// bytes, so a second `if err != nil` here could never fire — a branch nothing can observe,
	// which a mutation control proved by removing it with everything green. The parse is
	// repeated only because that helper does not hand back its AST.
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, filename, src, 0)
	if f == nil {
		return nil, fmt.Errorf("%s parsed for registrations but not for gate wiring", filename)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == batchRegFunc && len(call.Args) >= 2 {
			w.settleWired = types.ExprString(call.Args[1])
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || types.ExprString(sel.X) != batchGateVar || len(call.Args) < 3 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		p, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			return true
		}
		w.gated = append(w.gated, gatedBatchRoute{
			verb:    sel.Sel.Name,
			path:    p,
			handler: types.ExprString(call.Args[2]),
			line:    fset.Position(call.Pos()).Line,
		})
		return true
	})
	return w, nil
}

// gatedRoute returns the gate registration for verb+path, and whether it exists.
func (w *batchWiring) gatedRoute(verb, path string) (gatedBatchRoute, bool) {
	for _, g := range w.gated {
		if g.verb == verb && g.path == path {
			return g, true
		}
	}
	return gatedBatchRoute{}, false
}
