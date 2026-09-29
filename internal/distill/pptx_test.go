package distill

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// pptxDeck builds a minimal .pptx whose slides are listed in presentation.xml in the given order.
func pptxDeck(t *testing.T, order []int, slides map[int][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	var ids, rels strings.Builder
	for i, n := range order {
		fmt.Fprintf(&ids, `<p:sldId id="%d" r:id="rId%d"/>`, 256+i, n)
		fmt.Fprintf(&rels, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide%d.xml"/>`, n, n)
	}
	put("ppt/presentation.xml", `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>`+ids.String()+`</p:sldIdLst></p:presentation>`)
	put("ppt/_rels/presentation.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+rels.String()+`</Relationships>`)
	for n, s := range slides {
		put(fmt.Sprintf("ppt/slides/slide%d.xml", n), `<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><p:cSld><p:spTree>`+
			`<p:sp><p:nvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>`+s[0]+`</a:t></a:r></a:p></p:txBody></p:sp>`+
			`<p:sp><p:nvSpPr><p:nvPr><p:ph idx="1"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>`+s[1]+`</a:t></a:r><a:br/><a:r><a:t>next line</a:t></a:r></a:p></p:txBody></p:sp>`+
			`</p:spTree></p:cSld></p:sld>`)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// B18.13 — a slide deck is detected and converted to its slides' text, in presentation order.
func TestPPTX_ASlideDeckBecomesItsSlidesTextInPresentationOrder(t *testing.T) {
	deck := pptxDeck(t, []int{2, 1}, map[int][2]string{1: {"Revenue", "Up 40% on last year"}, 2: {"Agenda", "Why we are here"}})
	if f := DetectFormat(deck); f != FormatPPTX {
		t.Fatalf("detected %q, want pptx", f)
	}
	if f, ok := FormatFromMediaType("application/vnd.openxmlformats-officedocument.presentationml.presentation"); !ok || f != FormatPPTX {
		t.Fatalf("media type maps to %q, %v", f, ok)
	}
	res, err := (pptxConverter{}).Convert(context.Background(), deck)
	if err != nil {
		t.Fatal(err)
	}
	want := "## Slide 1 — Agenda\n\nWhy we are here next line\n\n## Slide 2 — Revenue\n\nUp 40% on last year next line"
	if res.Markdown != want {
		t.Errorf("converted to %q, want %q", res.Markdown, want)
	}
}
