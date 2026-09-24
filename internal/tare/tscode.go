package tare

// tscode.go — Tare phase 1b, the TypeScript half of the structural code trimmer (B6.2).
//
// Same contract as gocode.go, and LOSSY for the same reason: imports, signatures and types come
// through BYTE-IDENTICAL, every function body is replaced by an announced elision, and the output is
// re-parsed before it is returned — a result that does not parse is a REFUSAL, never a broken file.
//
// ⚠ NOT tree-sitter, FOR THE REASON gocode.go MEASURED: every Go tree-sitter binding is CGO, and this
// repo builds with CGO_ENABLED=0. The parse checks use esbuild's TypeScript parser, which is pure Go.
//
// ⚠ esbuild EXPOSES NO AST, so FINDING the bodies is the token scanner below. It is conservative on
// purpose: a brace it cannot place as a function body is descended into, never elided, and it skips
// type positions (annotations, interfaces, aliases, generic arguments) wholesale, because `=> {` inside
// a type is a function TYPE returning an object type, and eliding that would drop a type while every
// parser still accepts the result. The re-parse catches a broken file; it cannot catch that, so the
// scanner has to.
//
// ⚠ .ts ONLY. JSX does not parse under the TypeScript loader, so a .tsx file is refused, whole.

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/evanw/esbuild/pkg/api"
)

// ReasonNotTS is the TypeScript trimmer's refusal for input its parser rejects.
const ReasonNotTS = "content does not parse as TypeScript"

type TSBodyTrimmer struct{ observe func(Refusal) }

func NewTSBodyTrimmer() *TSBodyTrimmer { return &TSBodyTrimmer{} }

func (g *TSBodyTrimmer) WithObserver(f func(Refusal)) *TSBodyTrimmer { g.observe = f; return g }

func (g *TSBodyTrimmer) refuse(content []byte, kind Kind, reason string) ([]byte, int, int, error) {
	if g.observe != nil {
		g.observe(Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := EstimateTokens(content)
	return content, t, t, nil
}

// Reduce implements Reduction.
func (g *TSBodyTrimmer) Reduce(_ context.Context, content []byte, kind Kind) ([]byte, int, int, error) {
	if kind != KindCode {
		return g.refuse(content, kind, ReasonWrongKind)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return g.refuse(content, kind, ReasonEmpty)
	}
	if !ParsesAsTypeScript(content) {
		return g.refuse(content, kind, ReasonNotTS)
	}
	s, ok := newTSScan(content)
	if !ok {
		return g.refuse(content, kind, ReasonNotTS)
	}
	s.run()

	var cuts []elision
	for _, b := range s.bodies {
		lo, hi := s.t[b[0]].lo, s.t[b[1]].lo // hi is the closing brace, inclusive
		lines := bytes.Count(content[lo:hi], []byte("\n"))
		if lines < 1 {
			lines = 1
		}
		rep := fmt.Sprintf("{ %s%d %s elided */ }", ElisionMarker, lines, plural(lines))
		// Per-body, as in gocode.go: a body shorter than the marker would GROW.
		if len(rep) >= hi-lo+1 {
			continue
		}
		cuts = append(cuts, elision{lo: lo, hi: hi, lines: lines, replacement: rep})
	}
	if len(cuts) == 0 {
		if len(s.bodies) > 0 {
			return g.refuse(content, kind, ReasonBodiesTooSmall)
		}
		return g.refuse(content, kind, ReasonNoBodies)
	}

	sort.Slice(cuts, func(i, j int) bool { return cuts[i].lo > cuts[j].lo })
	out := append([]byte(nil), content...)
	for _, c := range cuts {
		out = append(out[:c.lo], append([]byte(c.replacement), out[c.hi+1:]...)...)
	}
	if len(out) >= len(content) {
		return g.refuse(content, kind, ReasonNotSmaller)
	}
	if !ParsesAsTypeScript(out) {
		return g.refuse(content, kind, ReasonReencodeFailed)
	}
	return out, EstimateTokens(content), EstimateTokens(out), nil
}

// ParsesAsTypeScript reports whether esbuild's TypeScript parser accepts src. Exported so a test
// can hold the trimmer's output to the same parser the trimmer holds itself to.
func ParsesAsTypeScript(src []byte) bool {
	r := api.Transform(string(src), api.TransformOptions{Loader: api.LoaderTS, LogLevel: api.LogLevelSilent})
	return len(r.Errors) == 0
}

// ─── the lexer ──────────────────────────────────────────────────────────────────────────────────

type tsTok struct {
	lo, hi int    // byte range [lo, hi)
	text   string // identifiers and punctuation; "" for atoms
	atom   bool   // string, template, regex or number — opaque, never scanned inside
	nl     bool   // a line break precedes this token
}

func tsIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func tsIdentPart(c byte) bool { return tsIdentStart(c) || (c >= '0' && c <= '9') }
func tsIsIdent(s string) bool { return s != "" && tsIdentStart(s[0]) }

// After these keywords a `/` starts a regex, not a division.
var tsRegexAfter = map[string]bool{"return": true, "typeof": true, "instanceof": true, "in": true,
	"of": true, "new": true, "delete": true, "void": true, "throw": true, "case": true, "do": true,
	"else": true, "yield": true, "await": true, "extends": true}

func lexTS(src []byte) ([]tsTok, bool) {
	var toks []tsTok
	n, i, nl := len(src), 0, false
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			nl = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
			continue
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		case c == '/' && i+1 < n && src[i+1] == '*':
			j := bytes.Index(src[i+2:], []byte("*/"))
			if j < 0 {
				return nil, false
			}
			if bytes.IndexByte(src[i:i+2+j], '\n') >= 0 {
				nl = true
			}
			i += j + 4
			continue
		}
		tok := tsTok{lo: i, nl: nl}
		nl = false
		var ok = true
		switch {
		case c == '"' || c == '\'':
			i, ok = tsSkipQuoted(src, i)
			tok.atom = true
		case c == '`':
			i, ok = tsSkipTemplate(src, i)
			tok.atom = true
		case (c >= '0' && c <= '9') || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9'):
			for i < n && (tsIdentPart(src[i]) || src[i] == '.') {
				i++
			}
			tok.atom = true
		case tsIdentStart(c):
			for i < n && tsIdentPart(src[i]) {
				i++
			}
			tok.text = string(src[tok.lo:i])
		case c == '/' && tsRegexAllowed(toks):
			i, ok = tsSkipRegex(src, i)
			tok.atom = true
		case c == '=' && i+1 < n && src[i+1] == '>':
			i += 2
			tok.text = "=>"
		case c == '?' && i+1 < n && (src[i+1] == '?' || (src[i+1] == '.' && !(i+2 < n && src[i+2] >= '0' && src[i+2] <= '9'))):
			i += 2
			tok.text = string(src[tok.lo:i])
		default:
			i++
			tok.text = string(c)
		}
		if !ok {
			return nil, false
		}
		tok.hi = i
		toks = append(toks, tok)
	}
	return toks, true
}

func tsRegexAllowed(toks []tsTok) bool {
	if len(toks) == 0 {
		return true
	}
	p := toks[len(toks)-1]
	if p.atom {
		return false
	}
	if tsIsIdent(p.text) {
		return tsRegexAfter[p.text]
	}
	return p.text != ")" && p.text != "]" && p.text != "}"
}

func tsSkipQuoted(src []byte, i int) (int, bool) {
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case q:
			return j + 1, true
		case '\n':
			return 0, false
		}
	}
	return 0, false
}

func tsSkipTemplate(src []byte, i int) (int, bool) {
	for j := i + 1; j < len(src); {
		switch {
		case src[j] == '\\':
			j += 2
		case src[j] == '`':
			return j + 1, true
		case src[j] == '$' && j+1 < len(src) && src[j+1] == '{':
			k, ok := tsSkipInterpolation(src, j+2)
			if !ok {
				return 0, false
			}
			j = k
		default:
			j++
		}
	}
	return 0, false
}

// tsSkipInterpolation skips a template's `${ … }` to just past its closing brace. Functions inside
// an interpolation are never elided: the whole template is one opaque atom.
func tsSkipInterpolation(src []byte, j int) (int, bool) {
	depth := 1
	for j < len(src) {
		c := src[j]
		var ok = true
		switch {
		case c == '"' || c == '\'':
			j, ok = tsSkipQuoted(src, j)
		case c == '`':
			j, ok = tsSkipTemplate(src, j)
		case c == '/' && j+1 < len(src) && src[j+1] == '/':
			for j < len(src) && src[j] != '\n' {
				j++
			}
		case c == '/' && j+1 < len(src) && src[j+1] == '*':
			k := bytes.Index(src[j+2:], []byte("*/"))
			if k < 0 {
				return 0, false
			}
			j += k + 4
		case c == '{':
			depth++
			j++
		case c == '}':
			depth--
			j++
			if depth == 0 {
				return j, true
			}
		default:
			j++
		}
		if !ok {
			return 0, false
		}
	}
	return 0, false
}

func tsSkipRegex(src []byte, i int) (int, bool) {
	inClass := false
	for j := i + 1; j < len(src); j++ {
		switch c := src[j]; {
		case c == '\\':
			j++
		case c == '\n':
			return 0, false
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '/':
			j++
			for j < len(src) && tsIdentPart(src[j]) {
				j++
			}
			return j, true
		}
	}
	return 0, false
}

// ─── the body finder ────────────────────────────────────────────────────────────────────────────

type tsFrame struct {
	kind    byte // '{' block or module, 'o' object literal, 'c' class body, '(' or '['
	ternary int  // unmatched `?` — the next `:` belongs to it
	caseLbl bool // a `case`/`default` whose `:` is still to come
}

type tsScan struct {
	t         []tsTok
	m         []int       // index of the matching ( [ { or ) ] }, else -1
	angleOpen map[int]int // `>` index → its `<`, for generic arguments the scanner skipped
	stack     []tsFrame
	pending   byte // 'c' a class header, 't' an interface/enum/declare header, awaiting its `{`
	pendingAt int  // the stack depth that header was opened at
	bodies    [][2]int
}

func newTSScan(src []byte) (*tsScan, bool) {
	toks, ok := lexTS(src)
	if !ok {
		return nil, false
	}
	m := make([]int, len(toks))
	var open []int
	pair := map[string]string{")": "(", "]": "[", "}": "{"}
	for i, tk := range toks {
		m[i] = -1
		switch tk.text {
		case "(", "[", "{":
			open = append(open, i)
		case ")", "]", "}":
			if len(open) == 0 || toks[open[len(open)-1]].text != pair[tk.text] {
				return nil, false
			}
			o := open[len(open)-1]
			open = open[:len(open)-1]
			m[o], m[i] = i, o
		}
	}
	if len(open) != 0 {
		return nil, false
	}
	return &tsScan{t: toks, m: m, angleOpen: map[int]int{}, stack: []tsFrame{{kind: '{'}}}, true
}

func (s *tsScan) text(i int) string {
	if i < 0 || i >= len(s.t) {
		return ""
	}
	return s.t[i].text
}

func (s *tsScan) top() *tsFrame { return &s.stack[len(s.stack)-1] }

func (s *tsScan) run() {
	for i := 0; i < len(s.t); {
		i = s.step(i)
	}
}

// Keywords whose `( … ) {` is a control-flow block, not a function.
var tsControl = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"with": true, "await": true}

// A `{` after one of these opens an object literal (value position), not a block.
var tsObjectAfter = map[string]bool{"=": true, "(": true, ",": true, ":": true, "[": true, "?": true,
	"??": true, "|": true, "&": true, "!": true, ".": true, "+": true, "-": true, "*": true, "%": true,
	"<": true, ">": true, "~": true, "^": true, "return": true, "yield": true, "await": true,
	"default": true, "throw": true, "in": true, "of": true, "typeof": true, "void": true, "delete": true}

func (s *tsScan) atStatementStart(i int) bool {
	if i == 0 || s.t[i].nl {
		return true
	}
	switch s.text(i - 1) {
	case ";", "{", "}", "export", "declare", "default", "const":
		return true
	}
	return false
}

func (s *tsScan) step(i int) int {
	tk := s.t[i]
	switch tk.text {
	case "{":
		if s.isBody(i) {
			s.bodies = append(s.bodies, [2]int{i, s.m[i]})
			return s.m[i] + 1
		}
		if s.pending != 0 && s.pendingAt == len(s.stack) {
			p := s.pending
			s.pending = 0
			if p == 't' {
				return s.m[i] + 1 // interface / enum / declare block: types only, never scanned
			}
			s.stack = append(s.stack, tsFrame{kind: 'c'})
			return i + 1
		}
		kind := byte('{')
		if tsObjectAfter[s.text(i-1)] {
			kind = 'o'
		}
		s.stack = append(s.stack, tsFrame{kind: kind})
		return i + 1
	case "(", "[":
		s.stack = append(s.stack, tsFrame{kind: tk.text[0]})
		return i + 1
	case ")", "]", "}":
		if len(s.stack) > 1 {
			s.stack = s.stack[:len(s.stack)-1]
		}
		return i + 1
	case "?":
		// `a?: T` is an optional marker, and so is `m?()` in a class body; anything else is a ternary.
		if n := s.text(i + 1); n != ":" && !(n == "(" && s.top().kind == 'c') {
			s.top().ternary++
		}
		return i + 1
	case ":":
		f := s.top()
		if f.ternary > 0 {
			f.ternary--
			return i + 1
		}
		if f.caseLbl {
			f.caseLbl = false
			return i + 1
		}
		prev := s.text(i - 1)
		if prev == ")" {
			// A return type. Skip it, and if a `{` follows it, that brace is the body.
			j := s.skipType(i + 1)
			if s.text(j) == "{" && s.isFuncParen(i-1) {
				s.bodies = append(s.bodies, [2]int{j, s.m[j]})
				return s.m[j] + 1
			}
			return j
		}
		isType := f.kind == '(' || f.kind == '[' || f.kind == 'c' || prev == "?"
		if f.kind == '{' {
			pp := s.text(i - 2)
			isType = isType || prev == "}" || prev == "]" ||
				(tsIsIdent(prev) && (pp == "let" || pp == "const" || pp == "var" || pp == ","))
		}
		if isType {
			return s.skipType(i + 1)
		}
		return i + 1
	case "case":
		s.top().caseLbl = true
		return i + 1
	case "default":
		if s.text(i+1) == ":" {
			s.top().caseLbl = true
		}
		return i + 1
	case "as", "satisfies", "extends", "implements":
		if s.text(i-1) != "." && i > 0 {
			return s.skipType(i + 1)
		}
		return i + 1
	case "class":
		if s.text(i-1) != "." {
			s.pending, s.pendingAt = 'c', len(s.stack)
		}
		return i + 1
	case "interface", "enum":
		if s.atStatementStart(i) && tsIsIdent(s.text(i+1)) {
			s.pending, s.pendingAt = 't', len(s.stack)
		}
		return i + 1
	case "declare":
		if n := s.text(i + 1); n == "module" || n == "global" || n == "namespace" {
			s.pending, s.pendingAt = 't', len(s.stack)
		}
		return i + 1
	case "type":
		// `type Name<…> = <type>` — the whole right-hand side is a type.
		if s.atStatementStart(i) && tsIsIdent(s.text(i+1)) {
			j := i + 2
			if s.text(j) == "<" {
				if k := s.matchAngle(j); k > 0 {
					j = k + 1
				}
			}
			if s.text(j) == "=" {
				return s.skipType(j + 1)
			}
		}
		return i + 1
	}
	// Generic arguments or parameters: `f<T>(…)`, `new Map<K, V>()`, and any `<…>` in a class or
	// interface header. Skipped as a type so a `{` inside them is never seen as code.
	if tsIsIdent(tk.text) && s.text(i+1) == "<" {
		if k := s.matchAngle(i + 1); k > 0 {
			next := s.t[min(k+1, len(s.t)-1)]
			if s.pending != 0 || next.text == "(" || (next.atom && k+1 < len(s.t)) {
				s.angleOpen[k] = i + 1
				return k + 1
			}
		}
	}
	return i + 1
}

// isBody reports whether the `{` at i opens a function body.
func (s *tsScan) isBody(i int) bool {
	switch s.text(i - 1) {
	case "=>":
		return true
	case ")":
		return s.isFuncParen(i - 1)
	}
	return false
}

// isFuncParen reports whether the `)` at c closes a function or method parameter list — as opposed
// to `if (…)`, `extends mixin(…)`, or a call.
func (s *tsScan) isFuncParen(c int) bool {
	q := s.m[c] - 1
	if q < 0 {
		return false
	}
	if s.t[q].atom || s.text(q) == "]" || s.text(q) == "function" || s.text(q) == "*" {
		return true // "name"() {}, [computed]() {}, function () {}, function* () {}
	}
	if s.text(q) == ">" {
		o, ok := s.angleOpen[q]
		if !ok {
			return false
		}
		q = o - 1
	}
	name := s.text(q)
	if !tsIsIdent(name) || tsControl[name] {
		return false
	}
	switch s.text(q - 1) {
	case ".", "?.", "extends", "implements", "new":
		return false
	}
	return true
}

// matchAngle returns the index of the `>` closing the `<` at i, or -1 if it is not a balanced group
// of type arguments.
func (s *tsScan) matchAngle(i int) int {
	depth := 0
	for j := i; j < len(s.t) && j < i+512; j++ {
		switch s.text(j) {
		case "<":
			depth++
		case ">":
			depth--
			if depth == 0 {
				return j
			}
		case "(", "[", "{":
			j = s.m[j]
		case ";", ")", "]", "}":
			return -1
		}
	}
	return -1
}

// skipType returns the index of the first token after the type starting at i.
func (s *tsScan) skipType(i int) int {
	expect, extends, cond := true, 0, 0
	for i < len(s.t) {
		tk := s.t[i]
		x := tk.text
		if expect {
			switch {
			case tk.atom:
				i++
				expect = false
			case x == "{" || x == "(" || x == "[":
				i = s.m[i] + 1
				if x == "(" && s.text(i) == "=>" { // function type: `(…) => R`
					i++
					continue
				}
				expect = false
			case x == "<": // generic function type: `<T>(…) => R`
				k := s.matchAngle(i)
				if k < 0 {
					return i
				}
				i = k + 1
			case x == "|" || x == "&" || x == "-" || x == "+" || x == "typeof" || x == "keyof" ||
				x == "unique" || x == "readonly" || x == "infer" || x == "new" || x == "abstract" ||
				x == "asserts":
				i++
			case tsIsIdent(x):
				i++
				expect = false
			default:
				return i
			}
			continue
		}
		// A complete type so far. A line break ends it unless the next token plainly continues it.
		if tk.nl && x != "|" && x != "&" && x != "." && x != "extends" && x != "is" &&
			!(x == "?" && extends > 0) && !(x == ":" && cond > 0) {
			return i
		}
		switch {
		case x == "." || x == "|" || x == "&" || x == "is":
			i++
			expect = true
		case x == "extends":
			extends++
			i++
			expect = true
		case x == "?" && extends > 0: // conditional type
			extends--
			cond++
			i++
			expect = true
		case x == ":" && cond > 0:
			cond--
			i++
			expect = true
		case x == "<":
			k := s.matchAngle(i)
			if k < 0 {
				return i
			}
			i = k + 1
		case x == "[":
			i = s.m[i] + 1
		default:
			return i
		}
	}
	return i
}
