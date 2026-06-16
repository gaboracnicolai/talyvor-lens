//go:build ignore

// gen_fixtures.go — DETERMINISTIC generator for the vision-OCR benchmark
// fixtures. Committed for provenance and reproducibility; it is OUT of the
// module build (the //go:build ignore constraint above) so golang.org/x/image
// never enters the lens go.mod.
//
// It renders each fixture's known text (heading + body + a small monospace
// "table") to a white image with the fixed basicfont 7x13 face (no external
// fonts → byte-deterministic), JPEG-encodes it at a fixed quality, and wraps the
// JPEG in a HAND-ROLLED minimal single-page IMAGE-ONLY PDF (one DCTDecode image
// XObject; a content stream that only places the image; NO text operators). The
// result is text-less by construction — pdf.go routes it to NeedsVision and a
// vision model must genuinely OCR the pixels (not read a text layer).
//
// To regenerate (x/image stays out of lens):
//
//	mkdir -p /tmp/visiongen && cp gen_fixtures.go /tmp/visiongen/main.go
//	cd /tmp/visiongen && sed -i '' '1,2d' main.go   # strip the build-ignore line
//	go mod init visiongen && go get golang.org/x/image/font/basicfont
//	go run . <abs path to internal/distill/testdata/vision>
//
// Each fixture's curated answer-relevant facts (the ground truth the benchmark
// grades OCR recovery against) are written to facts.json alongside the PDFs, so
// the generator is the single source of truth for both the rendered text and the
// facts.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// fixture is one synthetic document: the lines rendered into the image and the
// answer-relevant facts a correct answer would need (curated by construction,
// spanning heading / body-prose / table-cell so OCR loss in any region shows).
type fixture struct {
	File  string   `json:"file"`
	Lines []string `json:"-"`
	Facts []string `json:"facts"`
}

var fixtures = []fixture{
	{
		File: "invoice.pdf",
		Lines: []string{
			"INVOICE  INV-7781",
			"",
			"Bill to: Globex Corp",
			"Amount due: $4,200.00",
			"",
			"Item    | Qty | Unit",
			"Widget  | 12  | 350.00",
		},
		Facts: []string{"INV-7781", "Globex", "4,200.00", "350.00"},
	},
	{
		File: "report.pdf",
		Lines: []string{
			"Q3 FINANCIAL SUMMARY",
			"",
			"Revenue rose to 4.2M.",
			"Primary region: us-east-1.",
			"",
			"Metric | Value",
			"Churn  | 3.1%",
		},
		Facts: []string{"Q3 FINANCIAL SUMMARY", "4.2M", "us-east-1", "3.1%"},
	},
	{
		File: "memo.pdf",
		Lines: []string{
			"INCIDENT 4471",
			"",
			"Root cause: null deref.",
			"Affected 320 users.",
			"Duration: 12 minutes.",
		},
		Facts: []string{"INCIDENT 4471", "null deref", "320 users", "12 minutes"},
	},
	{
		File: "contract.pdf",
		Lines: []string{
			"AGREEMENT TERMS",
			"",
			"Late penalty: $50,000.",
			"Governing law: Delaware.",
			"",
			"Milestone | Due",
			"Final     | 2026-09-30",
		},
		Facts: []string{"50,000", "Delaware", "2026-09-30"},
	},
	{
		File: "inventory.pdf",
		Lines: []string{
			"STOCK REPORT",
			"",
			"SKU   | OnHand",
			"A100  | 4200",
			"B200  | 3100",
		},
		Facts: []string{"STOCK REPORT", "A100", "4200", "B200"},
	},
	{
		File: "roster.pdf",
		Lines: []string{
			"TEAM ROSTER",
			"",
			"Lead engineer: Dana Vex.",
			"",
			"Name | Role",
			"Kira | Eng",
			"Omar | Ops",
		},
		Facts: []string{"Dana Vex", "Kira", "Omar"},
	},
}

func renderImage(lines []string) image.Image {
	const cw, lh, pad = 7, 16, 12 // basicfont 7x13 advance; line pitch; margin
	maxChars := 1
	for _, l := range lines {
		if len(l) > maxChars {
			maxChars = len(l)
		}
	}
	w := pad*2 + maxChars*cw
	h := pad*2 + len(lines)*lh
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	d := &font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13}
	for i, l := range lines {
		d.Dot = fixed.P(pad, pad+lh*(i+1)-3) // baseline of line i
		d.DrawString(l)
	}
	return img
}

// imageOnlyPDF wraps a JPEG in a minimal single-page PDF with ONE image XObject
// and a content stream that only places it — no text operators, so the PDF
// carries no extractable text layer.
func imageOnlyPDF(jpg []byte, w, h int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	var off [6]int
	start := func(n int) { off[n] = buf.Len() }

	start(1)
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	start(2)
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	start(3)
	buf.WriteString(fmt.Sprintf("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << /XObject << /Im0 4 0 R >> >> /Contents 5 0 R >>\nendobj\n", w, h))
	start(4)
	buf.WriteString(fmt.Sprintf("4 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n", w, h, len(jpg)))
	buf.Write(jpg)
	buf.WriteString("\nendstream\nendobj\n")
	content := fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q\n", w, h)
	start(5)
	buf.WriteString(fmt.Sprintf("5 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n", len(content), content))

	xref := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		buf.WriteString(fmt.Sprintf("%010d 00000 n \n", off[i]))
	}
	buf.WriteString(fmt.Sprintf("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref))
	return buf.Bytes()
}

func main() {
	outDir := "."
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	for _, fx := range fixtures {
		img := renderImage(fx.Lines)
		var jbuf bytes.Buffer
		if err := jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 90}); err != nil {
			panic(err)
		}
		b := img.Bounds()
		pdf := imageOnlyPDF(jbuf.Bytes(), b.Dx(), b.Dy())
		if err := os.WriteFile(filepath.Join(outDir, fx.File), pdf, 0o644); err != nil {
			panic(err)
		}
		fmt.Printf("wrote %s (%d bytes, %dx%d)\n", fx.File, len(pdf), b.Dx(), b.Dy())
	}
	manifest, _ := json.MarshalIndent(fixtures, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "facts.json"), append(manifest, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote facts.json")
}
