package distill

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// DetectFormat best-effort identifies a document format from its bytes (magic
// numbers + structure), NOT a filename — the gateway receives bytes. It is
// deliberately conservative: when it can't tell, it returns the safest
// applicable format (text) rather than guessing. The future request-path
// integration should prefer the request's declared Content-Type via DistillAs
// when available; some formats (CSV vs plaintext) are not reliably
// distinguishable from bytes alone.
func DetectFormat(input []byte) Format {
	if len(input) == 0 {
		return FormatUnknown
	}

	// Binary container magic first — unambiguous.
	if bytes.HasPrefix(input, []byte("%PDF-")) {
		return FormatPDF
	}
	// OOXML (DOCX/XLSX) are ZIP archives ("PK\x03\x04"); disambiguate by the
	// part names inside rather than trusting the extension.
	if bytes.HasPrefix(input, []byte("PK\x03\x04")) {
		if f := detectZipOOXML(input); f != FormatUnknown {
			return f
		}
		return FormatUnknown
	}

	trimmed := bytes.TrimLeft(input, " \t\r\n\uFEFF")
	if len(trimmed) == 0 {
		return FormatText
	}

	// HTML before generic XML: an HTML doc is angle-bracketed but is not
	// well-formed XML, so check its distinctive markers first.
	if looksLikeHTML(trimmed) {
		return FormatHTML
	}

	switch trimmed[0] {
	case '{', '[':
		if json.Valid(trimmed) {
			return FormatJSON
		}
	case '<':
		// XML declaration or a root element. (HTML was already ruled out.)
		return FormatXML
	}

	// Nothing structural matched. If it's valid UTF-8 text, treat it as
	// plaintext/markdown passthrough; otherwise we don't know.
	if utf8.Valid(input) {
		return FormatText
	}
	return FormatUnknown
}

// detectZipOOXML peeks inside a ZIP for the part that identifies DOCX vs XLSX.
// Failure to open (truncated/corrupt zip) yields Unknown — the caller decides.
func detectZipOOXML(input []byte) Format {
	zr, err := openZip(input)
	if err != nil {
		return FormatUnknown
	}
	for _, f := range zr.File {
		switch f.Name {
		case "word/document.xml":
			return FormatDOCX
		case "xl/workbook.xml":
			return FormatXLSX
		}
	}
	return FormatUnknown
}

// looksLikeHTML checks the leading bytes (case-insensitively) for the markers
// that distinguish HTML from generic XML.
func looksLikeHTML(trimmed []byte) bool {
	// Look only at a bounded prefix — enough to see a doctype/root tag.
	head := trimmed
	if len(head) > 512 {
		head = head[:512]
	}
	lower := bytes.ToLower(head)
	for _, marker := range [][]byte{
		[]byte("<!doctype html"),
		[]byte("<html"),
		[]byte("<head"),
		[]byte("<body"),
	} {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}
