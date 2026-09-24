package tare_test

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// tsSample is the conformance sample, and it is the hard case on purpose: every `{` in a TYPE
// position — including `=> {` inside a function type — sits beside two real bodies. A scanner that
// treats `=> {` as an arrow body elides a type here, and every parser still accepts the result.
// ⚠ EVERY TYPE LITERAL IS LONGER THAN THE MARKER. A shorter one is kept by the size check even when
// it is misclassified, and the first draft of this sample passed with interface skipping switched off.
const tsSample = `import type { Req } from "./req";

export interface Handlers {
  onClick: (e: Event) => { handled: boolean; reason: string };
  nested: { deep: () => { x: number; y: number; label: string } };
}

export type Reply = (r: Req) => { status: number; body: string; retry: boolean };

type Cb = <T>(x: T) => { value: T; previous: T | undefined };

const table: Record<string, () => { ok: boolean; reason: string }> = {};

export class Router<T extends { id: string; name: string }> extends Base<{ tag: 1; kind: "router" }> implements Handlers {
  private cb: (x: number) => { y: number; label: string } = (x) => ({ y: x, label: "" });
  onClick = (e: Event): { handled: boolean; reason: string } => {
    const handled = e.defaultPrevented;
    console.log("clicked", handled);
    return { handled, reason: "click" };
  };
  route(path: string, opts: { method: "GET" | "POST"; retry?: () => { ms: number; tries: number } }): { matched: boolean; params: string[] } {
    const matched = path.startsWith("/");
    if (matched) {
      console.log(opts.method);
    }
    return { matched, params: [] };
  }
}
`

// keptVerbatim proves every byte outside an elision is the original's, in order: the output with
// each marker turned into "any brace-delimited span" must match the WHOLE original.
func keptVerbatim(t *testing.T, name string, orig, out []byte) {
	t.Helper()
	marker := regexp.MustCompile(`\{ /\* tare: \d+ lines? elided \*/ \}`)
	var pat strings.Builder
	pat.WriteString(`^`)
	last := 0
	for _, loc := range marker.FindAllIndex(out, -1) {
		pat.WriteString(regexp.QuoteMeta(string(out[last:loc[0]])))
		pat.WriteString(`\{[\s\S]*?\}`)
		last = loc[1]
	}
	pat.WriteString(regexp.QuoteMeta(string(out[last:])) + `$`)
	if !regexp.MustCompile(pat.String()).Match(orig) {
		t.Errorf("%s: the kept regions are not the original's bytes — something other than a body changed", name)
	}
}

func trimTS(t *testing.T, src []byte) []byte {
	t.Helper()
	out, _, _, err := tare.NewTSBodyTrimmer().Reduce(context.Background(), src, tare.KindCode)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if !tare.ParsesAsTypeScript(out) {
		t.Fatalf("output does not re-parse as TypeScript:\n%s", out)
	}
	return out
}

// B6.2 DONE: a trimmed TypeScript file re-parses with imports, signatures and types intact —
// measured on the SDK this repo ships, not on a fixture written to pass.
func TestTS_RealSDKFiles_KeepImportsSignaturesTypes(t *testing.T) {
	for _, tc := range []struct {
		file string
		keep []string // imports, types and signatures that must survive byte-for-byte
		gone []string // body statements that must not
	}{
		{"client.ts",
			[]string{
				`import { injectLensHeaders } from "./middleware";`,
				"export interface LensClientOptions {\n  lensUrl: string;",
				"private readonly headers: Record<string, string>;",
				"constructor(options: LensClientOptions) { /* tare: ",
				"getHeaders(): Record<string, string> { /* tare: ",
				"withSession(sessionId: string, agentName?: string): LensClient { /* tare: ",
				"private derive(overrides: Partial<LensClientOptions>): LensClient { /* tare: ",
			},
			[]string{`throw new Error("lensUrl is required")`, "Object.entries(this.headers)"}},
		{"middleware.ts",
			[]string{
				"import {\n  HEADER_AGENT,",
				"export interface InjectHeadersOptions {",
				"const OPTIONAL_HEADER_MAP: ReadonlyArray<\n  readonly [keyof InjectHeadersOptions, string]\n> = [",
				"export function injectLensHeaders(\n  existing: Record<string, string> = {},\n  options: InjectHeadersOptions,\n): Record<string, string> { /* tare: ",
			},
			[]string{"headers[HEADER_WORKSPACE] ="}},
	} {
		src, err := os.ReadFile("../../sdk/typescript/src/" + tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		out := trimTS(t, src)
		keptVerbatim(t, tc.file, src, out)
		for _, k := range tc.keep {
			if !bytes.Contains(out, []byte(k)) {
				t.Errorf("%s: lost %q", tc.file, k)
			}
		}
		for _, g := range tc.gone {
			if bytes.Contains(out, []byte(g)) {
				t.Errorf("%s: body statement %q survived", tc.file, g)
			}
		}
		t.Logf("MEASURED — sdk/typescript/src/%s: %d -> %d bytes (%.2f%%), %d lines elided",
			tc.file, len(src), len(out), 100*float64(len(src)-len(out))/float64(len(src)), tare.TrimmedLineCount(out))
	}
}

// Every `{` in a type position survives; only the two real bodies go.
func TestTS_TypesAreNeverElided(t *testing.T) {
	out := trimTS(t, []byte(tsSample))
	keptVerbatim(t, "tsSample", []byte(tsSample), out)
	for _, k := range []string{
		"onClick: (e: Event) => { handled: boolean; reason: string };",
		"nested: { deep: () => { x: number; y: number; label: string } };",
		"export type Reply = (r: Req) => { status: number; body: string; retry: boolean };",
		"type Cb = <T>(x: T) => { value: T; previous: T | undefined };",
		"const table: Record<string, () => { ok: boolean; reason: string }> = {};",
		`export class Router<T extends { id: string; name: string }> extends Base<{ tag: 1; kind: "router" }> implements Handlers {`,
		`private cb: (x: number) => { y: number; label: string } = (x) => ({ y: x, label: "" });`,
		"onClick = (e: Event): { handled: boolean; reason: string } => { /* tare: 4 lines elided */ };",
		`route(path: string, opts: { method: "GET" | "POST"; retry?: () => { ms: number; tries: number } }): { matched: boolean; params: string[] } { /* tare: 6 lines elided */ }`,
	} {
		if !bytes.Contains(out, []byte(k)) {
			t.Errorf("lost %q\n--- output ---\n%s", k, out)
		}
	}
	if n := bytes.Count(out, []byte(tare.ElisionMarker)); n != 2 {
		t.Errorf("%d elisions, want exactly the 2 function bodies\n%s", n, out)
	}
}

func TestTS_RefusesWhatItCannotTrim(t *testing.T) {
	types, err := os.ReadFile("../../sdk/typescript/src/types.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, src, reason string
	}{
		{"no bodies (sdk types.ts)", string(types), tare.ReasonNoBodies},
		{"bodies smaller than the marker", "export function f() { return 1 }\n", tare.ReasonBodiesTooSmall},
		{"Go source", "package p\n\nfunc F() int {\n\treturn 1\n}\n", tare.ReasonNotTS},
		{"TSX", "export const A = () => {\n  return <div className=\"a\">{x}</div>;\n};\n", tare.ReasonNotTS},
	} {
		var got string
		in := []byte(tc.src)
		out, tin, tout, err := tare.NewTSBodyTrimmer().WithObserver(func(r tare.Refusal) { got = r.Reason }).
			Reduce(context.Background(), in, tare.KindCode)
		if err != nil || !bytes.Equal(out, in) || tin != tout {
			t.Errorf("%s: refusal must return the input unchanged with nil error (err=%v, changed=%v)",
				tc.name, err, !bytes.Equal(out, in))
		}
		if got != tc.reason {
			t.Errorf("%s: refusal reason %q, want %q", tc.name, got, tc.reason)
		}
	}
}
