package distill

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestPDFResourceResidual_KNOWN is a TRIPWIRE, not a runnable case. It records
// the two PDF DoS residuals that CANNOT be bounded inside this leaf package:
//
//   - zlib-bomb OOM: ledongthuc inflates FlateDecode content streams internally
//     with no size cap; a 10 MiB PDF can expand to gigabytes. Go OOM is fatal —
//     recover() does NOT catch it.
//   - cyclic-ref hang / stack overflow: a cyclic page/object graph makes
//     GetPlainText loop forever or blow the stack; GetPlainText takes no ctx, so
//     even a timeout-context caller cannot interrupt it, and a goroutine+timeout
//     can only ABANDON (not kill) a runaway goroutine in Go — it keeps eating
//     memory/CPU toward process death.
//
// It is SKIPPED on purpose: actually constructing/running a bomb or cyclic PDF
// would OOM or hang CI. The fix is NOT in this package — it is the STAGE-3
// (request-path integration) requirement that ALL DISTILL conversion run under
// enforced resource isolation (a killable process/cgroup with memory + CPU +
// wall-clock limits) before untrusted request input reaches it. See the
// RESIDUAL RISK note in pdf.go and the "STAGE 3 BLOCKER" item in COORDINATION.md.
func TestPDFResourceResidual_KNOWN(t *testing.T) {
	t.Skip("known residual: zlib-bomb OOM + cyclic-ref hang are unbounded inside ledongthuc and uncatchable in-process (recover catches neither OOM nor stack overflow; Go cannot kill a runaway goroutine). MUST be handled by stage-3 resource isolation, not in-leaf — see pdf.go RESIDUAL RISK + COORDINATION.md STAGE 3 BLOCKER. This test is a tripwire only.")
}

// buildPDF constructs a minimal but structurally-valid single-page PDF with a
// correct xref table (offsets computed as the body is written). bodyLines are
// drawn as separate text lines; an empty bodyLines yields a page with NO text
// operators — the text-less / scanned case. Kept in-test (like the DOCX/XLSX
// builders) so the fixture stays reviewable rather than an opaque binary.
func buildPDF(bodyLines ...string) []byte {
	var content strings.Builder
	if len(bodyLines) > 0 {
		y := 700
		for _, ln := range bodyLines {
			fmt.Fprintf(&content, "BT /F1 24 Tf 72 %d Td (%s) Tj ET\n", y, escapePDFText(ln))
			y -= 30
		}
	}
	stream := content.String()

	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return buf.Bytes()
}

func escapePDFText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// TestPDFTextLayer: a PDF WITH a text layer extracts to plain Markdown. The
// bar is SOFTER than the stdlib formats (PDF extraction is lossy): assert the
// text content is present and roughly line-structured, not byte-exact.
func TestPDFTextLayer(t *testing.T) {
	ctx := context.Background()
	res, err := DistillAs(ctx, buildPDF("Hello DISTILL PDF", "Second line here"), FormatPDF)
	if err != nil {
		t.Fatalf("text PDF: unexpected err %v", err)
	}
	if res.NeedsVision {
		t.Errorf("a PDF with a text layer must NOT set NeedsVision; md=%q", res.Markdown)
	}
	if res.Format != FormatPDF {
		t.Errorf("Format = %q, want pdf", res.Format)
	}
	for _, want := range []string{"Hello DISTILL PDF", "Second line here"} {
		if !strings.Contains(res.Markdown, want) {
			t.Errorf("extracted text missing %q; got %q", want, res.Markdown)
		}
	}
	// Roughly line-structured: the two lines are not run together.
	if !strings.Contains(res.Markdown, "Hello DISTILL PDF\nSecond line here") {
		t.Errorf("expected the two text lines on separate lines; got %q", res.Markdown)
	}
	// Auto-detection path also reaches the PDF converter (via %PDF- magic).
	det, _ := Distill(ctx, buildPDF("Detected via magic"))
	if det.Format != FormatPDF || !strings.Contains(det.Markdown, "Detected via magic") {
		t.Errorf("Distill auto-detect of PDF failed: format=%q md=%q", det.Format, det.Markdown)
	}
}

// TestPDFTextLess: a PDF with NO text operators (the scanned/image-only case)
// must set NeedsVision=true with a warning — not empty-looking success, not
// garbage. This makes the core's reserved NeedsVision signal real.
func TestPDFTextLess(t *testing.T) {
	res, err := DistillAs(context.Background(), buildPDF(), FormatPDF) // no body → no text
	if err != nil {
		t.Fatalf("text-less PDF should not error, got %v", err)
	}
	if !res.NeedsVision {
		t.Errorf("text-less PDF must set NeedsVision=true; md=%q", res.Markdown)
	}
	if strings.TrimSpace(res.Markdown) != "" {
		t.Errorf("text-less PDF must not emit garbage Markdown; got %q", res.Markdown)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("text-less PDF should carry a warning explaining the NeedsVision route")
	}
}

// TestPDFMalformedNoPanic: malformed PDFs (a classic parser-exploit surface)
// must never panic/hang. ledongthuc can panic on some inputs — the converter's
// local recover turns that into a NeedsVision result. Reaching the end proves
// no panic escaped.
func TestPDFMalformedNoPanic(t *testing.T) {
	ctx := context.Background()
	cases := map[string][]byte{
		"not-a-pdf-body": []byte("%PDF-1.4\nthis is not a real pdf body"),
		"binary-garbage": []byte("%PDF-1.7\n%\xff\xff\xff garbage \x00\x00 trailer"),
		"null-padded":    append([]byte("%PDF-1.5\n"), bytes.Repeat([]byte{0x00}, 4096)...),
		"truncated":      buildPDF("ok")[:40], // truncated mid-structure
		"magic-only":     []byte("%PDF-"),
		// References /Encrypt → exercises ledongthuc's encrypted path (no
		// password → error or panic). Encrypted PDFs route to NeedsVision.
		"encrypted-ref": []byte("%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
			"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
			"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]>>endobj\n" +
			"trailer<</Root 1 0 R/Encrypt 4 0 R/Size 5>>\nstartxref\n0\n%%EOF"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := DistillAs(ctx, in, FormatPDF) // must not panic / hang
			// Universal invariant: a malformed PDF must NEVER be reported as a
			// successful conversion with empty Markdown (that would be silent
			// data loss). It must either carry real text, set NeedsVision, or
			// return an error.
			if err == nil && !res.NeedsVision && strings.TrimSpace(res.Markdown) == "" {
				t.Errorf("empty-looking success on malformed input (silent loss): %q", res.Markdown)
			}
			if res.Format != FormatPDF {
				t.Errorf("Format = %q, want pdf", res.Format)
			}
		})
	}
}
