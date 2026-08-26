package tare

// gocode.go — Tare phase 1b: the structural code trimmer, for Go (W6.1.2).
//
// ⚠ THIS REDUCER IS LOSSY, AND IT IS THE ONLY ONE IN THIS PACKAGE THAT IS. W6.1.2 says "KEEP
// imports, signatures and types; DROP BODIES" and, two lines later, "SAME RULES: lossless,
// refusable, wired to nothing". A dropped body is not recoverable from the output, so those cannot
// both hold. The behaviour instruction is the concrete one and is stated twice, so it is what is
// built — and this file says LOSSY out loud rather than filing the reducer under the other word.
// docs/tare-phase1b-measured.md carries the note.
//
// ⚠ SO THE ELISION ANNOUNCES ITSELF. A body replaced by `{}` tells a reading agent that the
// function IS EMPTY — not a smaller truth, a different and false one, which is the same class of
// harm as the previous compressor rewriting a prompt. Every elided body carries a marker and a line
// count, so an agent can decide whether to go and fetch the file.
//
// ── WHY Go'S STANDARD LIBRARY AND NOT tree-sitter, MEASURED ────────────────────────────────────
//
// W6.1.2 specifies tree-sitter and says talyvor-code "ALREADY SHIPS A SEMANTIC INDEX WITH AST
// TOOLING ... Reuse or say why you cannot". Measured, at talyvor-code and talyvor-lens:
//
//   · talyvor-code contains NO tree-sitter — no dependency, no binding, nothing to reuse. Its
//     semantic index (agent/internal/codebase/semindex_build.go) is a SHA-256 content-hash and a
//     text chunker. `go/ast` appears in that repo only inside TEST guards.
//   · ⚠ AND tree-sitter COULD NOT SHIP HERE ANYWAY. Every Go tree-sitter binding is CGO, and this
//     repo builds with CGO_ENABLED=0 in BOTH the CI build step (.github/workflows/ci.yaml) and the
//     Dockerfile. A CGO dependency would not compile, so a tree-sitter trimmer would run nowhere —
//     the same trap as building Tare into an Envoy that has never run.
//
// go/parser is in the standard library: no new dependency, no CGO, and the output's validity is
// checkable directly. It covers Go only; what a multi-language trimmer would cost is a decision,
// written up in the doc rather than guessed at here.
//
// ── WHY IT SPLICES SOURCE RATHER THAN PRINTING AN AST ──────────────────────────────────────────
//
// go/printer would re-emit the whole file, reformatting everything kept and moving comments around.
// Locating each body's byte range and splicing leaves every kept region BYTE-IDENTICAL — imports,
// types, signatures and their doc comments come through exactly as written, so a reader diffing the
// reduced form against the original sees only the elisions.

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// ElisionMarker is what every trimmed body carries. Exported so a consumer can detect a reduced
// file, and so the tests assert the real string rather than a copy of it.
const ElisionMarker = "/* tare: "

// Refusal reasons specific to the Go trimmer.
const (
	ReasonNotGo    = "content does not parse as Go"
	ReasonNoBodies = "no function bodies to elide"
	// ⚠ DISTINCT FROM ReasonNoBodies ON PURPOSE. "There are no bodies" and "every body is shorter
	// than the marker that would replace it" are different facts about a file, and a refusal
	// nobody can tell apart is one nobody can act on — the first says this file has nothing to
	// trim, the second says the marker is too expensive for code shaped like this.
	ReasonBodiesTooSmall = "every function body is smaller than the elision marker"
)

type GoBodyTrimmer struct{ observe func(Refusal) }

func NewGoBodyTrimmer() *GoBodyTrimmer { return &GoBodyTrimmer{} }

func (g *GoBodyTrimmer) WithObserver(f func(Refusal)) *GoBodyTrimmer { g.observe = f; return g }

func (g *GoBodyTrimmer) refuse(content []byte, kind Kind, reason string) ([]byte, int, int, error) {
	if g.observe != nil {
		g.observe(Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := EstimateTokens(content)
	return content, t, t, nil
}

type elision struct {
	lo, hi      int // byte range of the body, INCLUSIVE of both braces
	lines       int
	replacement string
}

// Reduce implements Reduction.
func (g *GoBodyTrimmer) Reduce(_ context.Context, content []byte, kind Kind) ([]byte, int, int, error) {
	if kind != KindCode {
		return g.refuse(content, kind, ReasonWrongKind)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return g.refuse(content, kind, ReasonEmpty)
	}
	fset := token.NewFileSet()
	// ParseComments so comment positions are real; the splice keeps them either way, but a parse
	// that discards them can misreport a body's extent.
	file, err := parser.ParseFile(fset, "in.go", content, parser.ParseComments)
	if err != nil {
		return g.refuse(content, kind, ReasonNotGo)
	}

	var cuts []elision
	sawBody := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			// A declaration with no body (an assembly or external func) has nothing to elide.
			continue
		}
		sawBody = true
		lo := fset.Position(fn.Body.Lbrace).Offset
		hi := fset.Position(fn.Body.Rbrace).Offset
		// ⚠ DEFENCE-IN-DEPTH WITH NO REACHABLE TRIGGER, AND SAID SO RATHER THAN IMPLIED.
		// Control I5 in w612-gotrim-controls-k7v3.py replaces this condition with `false` and
		// NOTHING goes red — for any input that parsed, go/parser always reports sane brace
		// offsets, so the branch cannot be reached. It is kept because it is correct and free, and
		// because a future caller might hand this function a FileSet that did not produce the
		// source. It is NOT presented as a tested guard, and the control that cannot fire is
		// recorded in the harness instead of being quietly deleted. (Same handling as the
		// unreachable savepoint rollback in the Stripe webhook — see W4.6.1 step 1.)
		if lo < 0 || hi <= lo || hi >= len(content) {
			continue
		}
		lines := fset.Position(fn.Body.Rbrace).Line - fset.Position(fn.Body.Lbrace).Line
		if lines < 1 {
			lines = 1
		}
		rep := fmt.Sprintf("{ %s%d %s elided */ }", ElisionMarker, lines, plural(lines))
		// ⚠ PER-BODY, NOT JUST PER-FILE. A body shorter than the marker GROWS when replaced, and a
		// file of one-liners would grow overall. Leaving it alone is the correct outcome.
		if len(rep) >= hi-lo+1 {
			continue
		}
		cuts = append(cuts, elision{lo: lo, hi: hi, lines: lines, replacement: rep})
	}
	if len(cuts) == 0 {
		if sawBody {
			return g.refuse(content, kind, ReasonBodiesTooSmall)
		}
		return g.refuse(content, kind, ReasonNoBodies)
	}

	// Splice from the end so earlier offsets stay valid.
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].lo > cuts[j].lo })
	out := append([]byte(nil), content...)
	for _, c := range cuts {
		out = append(out[:c.lo], append([]byte(c.replacement), out[c.hi+1:]...)...)
	}

	if len(out) >= len(content) {
		return g.refuse(content, kind, ReasonNotSmaller)
	}
	// ⚠ THE OUTPUT IS RE-PARSED BEFORE IT IS RETURNED, not merely tested elsewhere. W6.1.2: "THE
	// OUTPUT MUST STAY SYNTACTICALLY VALID ... or an agent receives a file it cannot reason about."
	// A test proves it for the inputs a test has; this proves it for the input in front of it, and
	// turns any shape I did not anticipate into a REFUSAL rather than a broken file.
	if _, err := parser.ParseFile(token.NewFileSet(), "out.go", out, parser.ParseComments); err != nil {
		return g.refuse(content, kind, ReasonReencodeFailed)
	}
	return out, EstimateTokens(content), EstimateTokens(out), nil
}

func plural(n int) string {
	if n == 1 {
		return "line"
	}
	return "lines"
}

// TrimmedLineCount reports how many source lines a reduced file says were elided. It exists so a
// metering stage (phase 1d) can record what was dropped without re-deriving it, and so a reader can
// check a saving against the loss that produced it.
func TrimmedLineCount(reduced []byte) int {
	total := 0
	rest := string(reduced)
	for {
		i := strings.Index(rest, ElisionMarker)
		if i < 0 {
			return total
		}
		rest = rest[i+len(ElisionMarker):]
		j := strings.IndexByte(rest, ' ')
		if j < 0 {
			return total
		}
		var n int
		if _, err := fmt.Sscanf(rest[:j], "%d", &n); err == nil {
			total += n
		}
	}
}
