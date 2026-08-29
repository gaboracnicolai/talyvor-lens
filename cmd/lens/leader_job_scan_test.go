package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
)

// leader_job_scan_test.go — the shared STRUCTURAL reader for main.go's leader-gated
// background workers.
//
// ⚠ WHY THIS EXISTS. Two guards over main.go decided "is this singleton worker wired,
// and is it gated" by looking for a LINE containing both `haComps.leader.Run` and the
// job's quoted name, then scanning a few lines UP for an `if`. Measured against the
// real guards (arms in ~/talyvor-queue/w61-{auditwiring,econworker}-mutation-controls-h2r7.py):
//
//   - deleting a registration outright  → CAUGHT (the guards do work: the positive control)
//   - commenting the registration OUT    → MISSED: the job never starts, both tokens survive
//     in the commented line, and the guard stays green
//   - a doc comment naming the job       → MISSED
//   - `map[string]string{"haComps.leader.Run": "audit-retention"}` → MISSED
//   - `go auditRetention.StartSweeper(ctx, …)` with NO leader election at all, plus a
//     comment mentioning haComps.leader.Run → MISSED, and "exactly one instance runs
//     each" is the entire point of that guard
//   - an economy worker moved OUT of `if cfg.EconomyEnabled` with the gate left behind
//     as a comment, or quoted inside a log string → MISSED, i.e. an economy-state
//     ledger writer reported as stopped by the master kill switch while it runs
//
// Commenting a job out is the ORDINARY way to disable one, so the failure is reachable
// by the routine act of turning something off.
//
// ⚠ THE SPLIT IS THE SAME ONE #515 DREW: the STRUCTURAL questions (is this a call, what
// `if` conditions enclose it) go to go/parser; the one question genuinely ABOUT A STRING
// (which job is this) stays a string. Comments are not in the AST, so no comment and no
// string literal can answer a structural question here.

// leaderJob is one `haComps.leader.Run(ctx, "<name>", …)` CALL in main.go.
type leaderJob struct {
	name string // the job name: the call's second argument, which must be a string literal
	// conds are the `if` conditions that govern the call, outermost first, rendered from
	// the AST. An else-branch is rendered `!(cond)`.
	conds []string
}

func (j leaderJob) gatedOn(cond string) bool {
	for _, c := range j.conds {
		if c == cond {
			return true
		}
	}
	return false
}

// leaderRunSelector is the wiring call this repo starts every singleton background worker
// through. It is a NAME, so it stays a string — but it is matched against the rendered
// AST expression of the call's function, never against a line of the file.
const leaderRunSelector = "haComps.leader.Run"

// scanLeaderJobs parses src and returns every leader.Run registration it contains.
// Parse errors are RETURNED, never swallowed: a scanner that reports "no jobs" because
// the file did not parse is a guard that cannot fail.
func scanLeaderJobs(filename string, src []byte) ([]leaderJob, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var (
		out   []leaderJob
		stack []ast.Node
	)
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok || types.ExprString(call.Fun) != leaderRunSelector || len(call.Args) < 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out = append(out, leaderJob{name: name, conds: enclosingConds(stack)})
		return true
	})
	return out, nil
}

// enclosingConds walks the ancestor stack and renders the condition of every `if` whose
// BODY (or else-branch) contains the node. A call sitting in the `if`'s own CONDITION is
// not governed by it and contributes nothing.
func enclosingConds(stack []ast.Node) []string {
	var conds []string
	for i := 0; i+1 < len(stack); i++ {
		ifs, ok := stack[i].(*ast.IfStmt)
		if !ok {
			continue
		}
		switch child := stack[i+1]; {
		case ast.Node(ifs.Body) == child:
			conds = append(conds, types.ExprString(ifs.Cond))
		case ifs.Else != nil && ifs.Else == child:
			conds = append(conds, "!("+types.ExprString(ifs.Cond)+")")
		}
	}
	return conds
}

// callsOutsideLeaderJob returns the lines of every call to receiver.method in src that is NOT
// lexically inside a `haComps.leader.Run(ctx, "<jobName>", …)` call.
//
// ⚠ WHY LEXICALLY INSIDE AND NOT "IS THE JOB PRESENT". A guard asking only whether the job is
// registered cannot see a SECOND, ungated invocation of the same work — and a guard asking only
// whether an ungated line is absent is defeated by writing it a different way. Both halves of the
// cache-warmer guard were text rules of exactly those two shapes, and a closure walked past both.
func callsOutsideLeaderJob(filename string, src []byte, receiver, method, jobName string) ([]int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []int
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method || types.ExprString(sel.X) != receiver {
			return true
		}
		for _, anc := range stack[:len(stack)-1] {
			outer, ok := anc.(*ast.CallExpr)
			if !ok || types.ExprString(outer.Fun) != leaderRunSelector || len(outer.Args) < 2 {
				continue
			}
			lit, ok := outer.Args[1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if v, uerr := strconv.Unquote(lit.Value); uerr == nil && v == jobName {
				return true // inside the named leader job — this call is gated
			}
		}
		out = append(out, fset.Position(call.Pos()).Line)
		return true
	})
	return out, nil
}
