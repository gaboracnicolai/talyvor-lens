package distill

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestFenceBreakout: content containing its own triple-backtick run must NOT
// break out of the code fence — the opening fence grows longer than any run
// inside, so injected Markdown stays inert.
func TestFenceBreakout(t *testing.T) {
	ctx := context.Background()

	t.Run("json", func(t *testing.T) {
		in := []byte("{\"x\":\"```\\ninjected heading\"}")
		res, err := DistillAs(ctx, in, FormatJSON)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Markdown, "````json\n") {
			t.Errorf("fence must grow to 4+ backticks; got prefix %q", firstLine(res.Markdown))
		}
		if !strings.HasSuffix(res.Markdown, "\n````") {
			t.Errorf("closing fence must match (4 backticks); got %q", res.Markdown)
		}
	})

	t.Run("xml", func(t *testing.T) {
		in := []byte("<note>```not a real fence```</note>")
		res, err := DistillAs(ctx, in, FormatXML)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Markdown, "````xml\n") {
			t.Errorf("xml fence must grow past content backticks; got %q", firstLine(res.Markdown))
		}
	})
}

// TestDeepNestingNoStackOverflow: a pathologically deep HTML document must not
// crash (stack overflow is FATAL/unrecoverable in Go) — the depth-bounded
// walkers stop gracefully. Reaching the assertion at all proves no crash.
func TestDeepNestingNoStackOverflow(t *testing.T) {
	var sb strings.Builder
	const depth = 6000 // well past htmlMaxDepth
	for i := 0; i < depth; i++ {
		sb.WriteString("<div>")
	}
	sb.WriteString("deep text")
	for i := 0; i < depth; i++ {
		sb.WriteString("</div>")
	}
	// Must return (any result/error) without panicking or overflowing the stack.
	if _, err := DistillAs(context.Background(), []byte(sb.String()), FormatHTML); err != nil {
		t.Logf("deep HTML returned err (acceptable): %v", err)
	}
}

// TestHrefControlCharBypass: a scheme disguised with an embedded control char
// (java<TAB>script:) must still be dropped — sanitizeHref strips controls
// before inspecting the scheme.
func TestHrefControlCharBypass(t *testing.T) {
	in := []byte("<a href=\"java\tscript:alert(1)\">click me</a>")
	res, err := DistillAs(context.Background(), in, FormatHTML)
	if err != nil {
		t.Fatal(err)
	}
	low := strings.ToLower(res.Markdown)
	if strings.Contains(low, "javascript") || strings.Contains(low, "alert(") {
		t.Errorf("control-char scheme bypass leaked: %q", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "click me") {
		t.Errorf("link text should survive as plain text: %q", res.Markdown)
	}
}

// TestDistillMalformedZipNoPanic: Distill() runs DetectFormat (an untrusted ZIP
// parse) BEFORE conversion; a malformed ZIP must not panic out — the recover in
// convert() now spans detection. Reaching the end proves no panic escaped.
func TestDistillMalformedZipNoPanic(t *testing.T) {
	cases := [][]byte{
		append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0xff}, 256)...),
		append([]byte("PK\x03\x04"), make([]byte, 512)...),
		[]byte("PK\x03\x04"),
	}
	for _, in := range cases {
		_, _ = Distill(context.Background(), in) // must not panic
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
