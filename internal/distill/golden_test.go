package distill

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trimTrail normalizes trailing newlines so golden comparisons don't hinge on
// a file's final newline.
func trimTrail(s string) string { return strings.TrimRight(s, "\n") }

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return trimTrail(string(b))
}

// TestGoldenCorpus is the parity bar: each representative document converts to
// its hand-checked expected Markdown (structure preserved, noise stripped).
func TestGoldenCorpus(t *testing.T) {
	cases := []struct {
		name    string
		format  Format
		input   []byte // when nil, read from inputFile
		inputF  string
		expectF string
	}{
		{name: "html", format: FormatHTML, inputF: "sample.html", expectF: "sample.html.md"},
		{name: "csv", format: FormatCSV, inputF: "sample.csv", expectF: "sample.csv.md"},
		{name: "json", format: FormatJSON, inputF: "sample.json", expectF: "sample.json.md"},
		{name: "xml", format: FormatXML, inputF: "sample.xml", expectF: "sample.xml.md"},
		{name: "text", format: FormatText, inputF: "sample.txt", expectF: "sample.txt.md"},
		{name: "docx", format: FormatDOCX, input: buildDOCX(), expectF: "docx_expected.md"},
		{name: "xlsx", format: FormatXLSX, input: buildXLSX(), expectF: "xlsx_expected.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.input
			if in == nil {
				b, err := os.ReadFile(filepath.Join("testdata", tc.inputF))
				if err != nil {
					t.Fatalf("read input: %v", err)
				}
				in = b
			}
			res, err := DistillAs(context.Background(), in, tc.format)
			if err != nil {
				t.Fatalf("DistillAs(%s): %v", tc.format, err)
			}
			if res.Format != tc.format {
				t.Errorf("Result.Format = %q, want %q", res.Format, tc.format)
			}
			want := readGolden(t, tc.expectF)
			if got := trimTrail(res.Markdown); got != want {
				t.Errorf("Markdown mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", tc.name, got, want)
			}
		})
	}
}

// TestHTMLStripsActiveContent makes the security property explicit: scripts,
// styles, and event-handler attributes never appear in the output.
func TestHTMLStripsActiveContent(t *testing.T) {
	res, err := DistillAs(context.Background(), mustRead(t, "sample.html"), FormatHTML)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"alert(", "<script", "onclick", "color: red", "steal()"} {
		if strings.Contains(res.Markdown, banned) {
			t.Errorf("output leaked active content %q:\n%s", banned, res.Markdown)
		}
	}
}

// TestHTMLDropsDangerousHref confirms javascript:/data: link schemes are not
// carried into the Markdown.
func TestHTMLDropsDangerousHref(t *testing.T) {
	in := []byte(`<p><a href="javascript:steal()">click</a> and <a href="https://ok.com">ok</a></p>`)
	res, err := DistillAs(context.Background(), in, FormatHTML)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Markdown, "javascript:") {
		t.Errorf("javascript: href leaked: %s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "[ok](https://ok.com)") {
		t.Errorf("safe link should survive: %s", res.Markdown)
	}
	if !strings.Contains(res.Markdown, "click") {
		t.Errorf("dangerous-link TEXT should survive (just not the URL): %s", res.Markdown)
	}
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want Format
	}{
		{"html-doctype", []byte("<!DOCTYPE html><html><body>hi</body></html>"), FormatHTML},
		{"html-tag", []byte("  <html><head></head></html>"), FormatHTML},
		{"json-obj", []byte(`{"a":1}`), FormatJSON},
		{"json-arr", []byte("[1,2,3]"), FormatJSON},
		{"xml-decl", []byte(`<?xml version="1.0"?><a/>`), FormatXML},
		{"xml-root", []byte("<note><to>x</to></note>"), FormatXML},
		{"pdf", []byte("%PDF-1.4\n..."), FormatPDF},
		{"docx", buildDOCX(), FormatDOCX},
		{"xlsx", buildXLSX(), FormatXLSX},
		{"plain", []byte("just some words\nwith newlines"), FormatText},
		{"empty", []byte(""), FormatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectFormat(tc.in); got != tc.want {
				t.Errorf("DetectFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSafety(t *testing.T) {
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		if _, err := Distill(ctx, nil); !errors.Is(err, ErrEmptyInput) {
			t.Errorf("empty input should be ErrEmptyInput, got %v", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		big := make([]byte, MaxInputBytes+1)
		if _, err := DistillAs(ctx, big, FormatText); !errors.Is(err, ErrTooLarge) {
			t.Errorf("oversize should be ErrTooLarge, got %v", err)
		}
	})

	t.Run("unsupported-format", func(t *testing.T) {
		if _, err := DistillAs(ctx, []byte("x"), FormatUnknown); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("unknown format should be ErrUnsupportedFormat, got %v", err)
		}
	})

	// Malformed inputs claimed as a structured format must error cleanly,
	// never panic (the recover backstop turns any panic into ErrConversionFailed).
	t.Run("malformed-no-panic", func(t *testing.T) {
		malformed := []struct {
			f  Format
			in []byte
		}{
			{FormatDOCX, []byte("not a zip at all")},
			{FormatXLSX, []byte("PK\x03\x04 truncated")},
			{FormatJSON, []byte(`{"unterminated":`)},
			{FormatXML, []byte("<a><b></a>")},
			{FormatHTML, []byte("<<<>>>malformed<<<")},
			{FormatCSV, []byte("a,\"unterminated")},
		}
		for _, m := range malformed {
			// Must return (not hang/panic); an error is fine, success is fine.
			_, _ = DistillAs(ctx, m.in, m.f)
		}
	})
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// ── in-test OOXML builders (keep the XML visible/reviewable rather than
// committing opaque binary fixtures) ──

func buildDOCX() []byte {
	const doc = `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Doc Title</w:t></w:r></w:p>
    <w:p><w:r><w:t>A paragraph of text.</w:t></w:r></w:p>
    <w:p><w:pPr><w:numPr><w:ilvl w:val="0"/></w:numPr></w:pPr><w:r><w:t>Bullet one</w:t></w:r></w:p>
    <w:p><w:pPr><w:numPr><w:ilvl w:val="0"/></w:numPr></w:pPr><w:r><w:t>Bullet two</w:t></w:r></w:p>
    <w:tbl>
      <w:tr><w:tc><w:p><w:r><w:t>H1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>H2</w:t></w:r></w:p></w:tc></w:tr>
      <w:tr><w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc></w:tr>
    </w:tbl>
  </w:body>
</w:document>`
	return zipBytes(map[string]string{"word/document.xml": doc})
}

func buildXLSX() []byte {
	const shared = `<?xml version="1.0" encoding="UTF-8"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <si><t>Name</t></si><si><t>Role</t></si><si><t>Age</t></si>
  <si><t>Alice</t></si><si><t>Engineer</t></si><si><t>Bob</t></si><si><t>Designer</t></si>
</sst>`
	const sheet = `<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>
    <row r="2"><c r="A2" t="s"><v>3</v></c><c r="B2" t="s"><v>4</v></c><c r="C2"><v>30</v></c></row>
    <row r="3"><c r="A3" t="s"><v>5</v></c><c r="B3" t="s"><v>6</v></c><c r="C3"><v>25</v></c></row>
  </sheetData>
</worksheet>`
	return zipBytes(map[string]string{
		"xl/workbook.xml":          `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`,
		"xl/sharedStrings.xml":     shared,
		"xl/worksheets/sheet1.xml": sheet,
	})
}

func zipBytes(parts map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range parts {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()
	return buf.Bytes()
}
