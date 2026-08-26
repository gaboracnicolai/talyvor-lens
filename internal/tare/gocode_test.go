package tare_test

// gocode_test.go — W6.1.2, Tare phase 1b: the structural code trimmer.
//
// ⚠ THIS REDUCER IS LOSSY AND THE ITEM'S OWN WORDING SAYS BOTH THINGS. W6.1.2 says "KEEP imports,
// signatures and types; DROP BODIES" and, two lines down, "SAME RULES: lossless, refusable, wired
// to nothing". A dropped body is not recoverable from the output, so those cannot both hold. The
// behaviour instruction is stated twice and concretely, so it is the one implemented — and the
// reducer is named, documented and TESTED as lossy rather than quietly filed under the other word.
// docs/tare-phase1b-measured.md carries the full note.
//
// ⚠ THE ELISION ANNOUNCES ITSELF, AND THAT IS THE DESIGN DECISION THAT MATTERS HERE. A body
// replaced by `{}` tells a reading agent the function IS EMPTY — which is not a smaller truth, it
// is a different and false one, exactly like the previous compressor rewriting a prompt. A body
// replaced by `{ /* tare: 12 lines elided */ }` tells it to go and ask. Every test below asserts
// the marker survives.

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

func trimGo(t *testing.T, src string) (out []byte, tin, tout int) {
	t.Helper()
	out, tin, tout, err := tare.NewGoBodyTrimmer().Reduce(context.Background(), []byte(src), tare.KindCode)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	return out, tin, tout
}

// mustReparse is the item's hard requirement: "THE OUTPUT MUST STAY SYNTACTICALLY VALID — assert it
// re-parses, per language, or an agent receives a file it cannot reason about."
func mustReparse(t *testing.T, out []byte) {
	t.Helper()
	if _, err := parser.ParseFile(token.NewFileSet(), "out.go", out, parser.ParseComments); err != nil {
		t.Fatalf("THE OUTPUT DOES NOT RE-PARSE — an agent would receive a file it cannot reason "+
			"about:\n%v\n---\n%s", err, out)
	}
}

const sampleGo = `package demo

import (
	"context"
	"fmt"
)

// Config is the thing an agent most needs to see.
type Config struct {
	Name    string
	Retries int
}

const DefaultRetries = 3

var ErrNope = fmt.Errorf("nope")

// Run does the work. The DOC COMMENT is signature-adjacent and must survive.
func Run(ctx context.Context, c Config) (int, error) {
	total := 0
	for i := 0; i < c.Retries; i++ {
		total += i
		if total > 100 {
			return total, ErrNope
		}
	}
	fmt.Println("a body line an agent does not need to plan with")
	return total, nil
}

func (c Config) String() string {
	return fmt.Sprintf("%s/%d", c.Name, c.Retries)
}
`

// ⚠ THE HEADLINE: signatures, types, imports and doc comments survive; bodies go.
func TestGo_KeepsImportsTypesAndSignaturesAndDropsBodies(t *testing.T) {
	out, tin, tout := trimGo(t, sampleGo)
	mustReparse(t, out)

	for _, keep := range []string{
		"package demo",
		`"context"`, `"fmt"`,
		"type Config struct", "Name    string", "Retries int",
		"const DefaultRetries = 3",
		"var ErrNope",
		"// Config is the thing an agent most needs to see.",
		"// Run does the work.",
		"func Run(ctx context.Context, c Config) (int, error)",
		"func (c Config) String() string",
	} {
		if !bytes.Contains(out, []byte(keep)) {
			t.Fatalf("%q is GONE. Imports, types, signatures and their doc comments are exactly what "+
				"the trimmer must keep — they are what an agent plans with.\n---\n%s", keep, out)
		}
	}
	for _, gone := range []string{
		"total += i",
		`fmt.Println("a body line an agent does not need to plan with")`,
		`fmt.Sprintf("%s/%d"`,
	} {
		if bytes.Contains(out, []byte(gone)) {
			t.Fatalf("body content %q survived — nothing was trimmed", gone)
		}
	}
	if tout >= tin {
		t.Fatalf("token estimate did not fall: %d -> %d", tin, tout)
	}
}

// ⚠ THE ELISION MUST BE VISIBLE. `{}` is a false statement about the function.
func TestGo_EveryElidedBodySaysSoInTheOutput(t *testing.T) {
	out, _, _ := trimGo(t, sampleGo)
	mustReparse(t, out)

	markers := bytes.Count(out, []byte(tare.ElisionMarker))
	if markers != 2 {
		t.Fatalf("found %d elision markers, want 2 (Run and String).\nA body replaced by a bare `{}` "+
			"tells a reading agent the function IS EMPTY — a different and FALSE statement, not a "+
			"smaller one.\n---\n%s", markers, out)
	}
	if bytes.Contains(out, []byte("{\n}")) || bytes.Contains(out, []byte("{}")) {
		t.Fatalf("an empty body block was emitted without a marker:\n%s", out)
	}
	// The count is part of the message: an agent deciding whether to fetch the file needs the size.
	if !bytes.Contains(out, []byte("lines elided")) {
		t.Fatalf("the marker does not say HOW MUCH was elided, which is what an agent needs to "+
			"decide whether to go and read the file:\n%s", out)
	}
}

// ⚠ EVERYTHING KEPT IS BYTE-IDENTICAL. The trimmer splices the source rather than re-printing an
// AST, so it cannot reformat, reorder or drop a comment it was supposed to keep.
func TestGo_KeptRegionsAreByteIdenticalNotReformatted(t *testing.T) {
	src := "package p\n\nimport   \"fmt\"   // deliberately odd spacing\n\ntype T struct{ A int }\n\nfunc F() { fmt.Println(1); fmt.Println(2); fmt.Println(3) }\n"
	out, _, _ := trimGo(t, src)
	mustReparse(t, out)
	if !bytes.Contains(out, []byte(`import   "fmt"   // deliberately odd spacing`)) {
		t.Fatalf("the kept region was REFORMATTED. Re-printing an AST would do that; splicing the "+
			"source cannot, and splicing is what keeps a diff reviewable.\n---\n%s", out)
	}
}

// ⚠ THE REFUSALS.
func TestGo_RefusesAndReturnsTheInputUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantReason string
		kind                 tare.Kind
	}{
		{"wrong kind", sampleGo, tare.ReasonWrongKind, tare.KindJSON},
		{"empty", "", tare.ReasonEmpty, tare.KindCode},
		{"not go", "def f():\n  return 1\n", tare.ReasonNotGo, tare.KindCode},
		{"no function bodies", "package p\n\ntype T struct{ A int }\n", tare.ReasonNoBodies, tare.KindCode},
		{"bodies too small to pay", "package p\n\nfunc a() { x() }\n", tare.ReasonBodiesTooSmall, tare.KindCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got tare.Refusal
			r := tare.NewGoBodyTrimmer().WithObserver(func(f tare.Refusal) { got = f })
			out, tin, tout, err := r.Reduce(context.Background(), []byte(tc.in), tc.kind)
			if err != nil {
				t.Fatalf("a refusal must not be an error: %v", err)
			}
			if !bytes.Equal(out, []byte(tc.in)) {
				t.Fatalf("a refusal must return the input UNCHANGED.\n in: %q\nout: %q", tc.in, out)
			}
			if tin != tout {
				t.Fatalf("a refusal changed the token estimate: %d -> %d", tin, tout)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// ⚠ A BODY THE MARKER WOULD NOT SHRINK IS LEFT ALONE. Trimming `{ x() }` to
// `{ /* tare: 1 line elided */ }` GROWS the file, which costs money instead of saving it.
func TestGo_LeavesBodiesTheMarkerWouldNotShrink(t *testing.T) {
	src := "package p\n\nfunc tiny() { x() }\n\nfunc big() {\n\ta()\n\tb()\n\tc()\n\td()\n\te()\n\tf()\n\tg()\n}\n"
	out, _, _ := trimGo(t, src)
	mustReparse(t, out)
	if !bytes.Contains(out, []byte("func tiny() { x() }")) {
		t.Fatalf("the tiny body was trimmed even though the marker is longer than it:\n%s", out)
	}
	if bytes.Contains(out, []byte("\td()")) {
		t.Fatalf("the large body was not trimmed:\n%s", out)
	}
	if len(out) >= len(src) {
		t.Fatalf("the file did not shrink: %d -> %d", len(src), len(out))
	}
}

func TestGo_IsDeterministic(t *testing.T) {
	first, _, _ := trimGo(t, sampleGo)
	if len(first) >= len(sampleGo) {
		t.Fatalf("nothing was trimmed (%d -> %d), so repeating it asserts only that a no-op is stable",
			len(sampleGo), len(first))
	}
	for i := 0; i < 20; i++ {
		again, _, _ := trimGo(t, sampleGo)
		if !bytes.Equal(first, again) {
			t.Fatalf("run %d differed", i)
		}
	}
}

// ⚠ MEASURED ON REAL CODE — this repository's own Go, which is what the item asks for.
func TestGoCorpus_ThisRepositorysOwnSource(t *testing.T) {
	files := goSourceFiles(t, "../..", 400)
	if len(files) < 100 {
		t.Fatalf("found %d Go files — the walk has gone blind and a figure over it would be a "+
			"statement about the search", len(files))
	}
	r := tare.NewGoBodyTrimmer()
	var bytesIn, bytesOut, trimmed, refused int
	for _, f := range files {
		out, _, _, err := r.Reduce(context.Background(), f.src, tare.KindCode)
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		bytesIn += len(f.src)
		bytesOut += len(out)
		if len(out) != len(f.src) {
			trimmed++
			// ⚠ EVERY REAL FILE RE-PARSES. Synthetic tests prove the trimmer handles shapes I
			// thought of; the repository is where the shape I did not think of lives.
			mustReparse(t, out)
		} else {
			refused++
		}
	}
	pct := 100 * float64(bytesIn-bytesOut) / float64(bytesIn)
	t.Logf("MEASURED — %d Go files in this repository (%d trimmed, %d refused): %d -> %d bytes (%.2f%%)",
		len(files), trimmed, refused, bytesIn, bytesOut, pct)
	if trimmed == 0 {
		t.Fatal("not one file was trimmed — the figure above would be a statement about the corpus")
	}
}

type goFile struct {
	path string
	src  []byte
}

func goSourceFiles(t *testing.T, root string, limit int) []goFile {
	t.Helper()
	var out []goFile
	err := walkGoSource(root, func(path, src string) {
		if len(out) >= limit || strings.Contains(path, "/sdk/") {
			return
		}
		out = append(out, goFile{path: path, src: []byte(src)})
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// walkGoSource visits every .go file under root. It walks the TREE rather than taking a list: the
// file that breaks a parser is the one nobody thought to list.
func walkGoSource(root string, visit func(path, src string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		visit(filepath.ToSlash(path), string(b))
		return nil
	})
}
