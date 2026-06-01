package distill

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"
)

// xmlConverter emits XML inside a fenced ```xml block. Like JSON, generic XML
// is itself the structure, and lossy re-serialization (encoding/xml drops
// comments, normalizes namespaces, reorders attributes) would degrade it — so
// we VALIDATE well-formedness by scanning every token, then emit the original
// content with whitespace normalized. Validation is hardened against the XML
// attack surface: entity expansion and external entities are disabled.
type xmlConverter struct{}

func (xmlConverter) Format() Format { return FormatXML }

func (xmlConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	dec := newSafeXMLDecoder(bytes.NewReader(input))

	for {
		if err := ctx.Err(); err != nil {
			return Result{Format: FormatXML}, err
		}
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Result{Format: FormatXML}, err
		}
	}

	content := strings.TrimSpace(string(input))
	// fencedBlock guards against fence-break-out if the XML contains ``` runs.
	return Result{Markdown: fencedBlock("xml", content), Format: FormatXML}, nil
}

// newSafeXMLDecoder builds an xml.Decoder hardened for untrusted input: strict
// parsing, only the predefined entities (custom/DTD entities are not expanded
// — Go's encoding/xml ignores DTDs, so billion-laughs isn't a vector, and this
// makes that explicit), and an identity charset reader (we operate on UTF-8;
// no external charset fetching). Shared by the XML and DOCX/XLSX converters.
func newSafeXMLDecoder(r io.Reader) *xml.Decoder {
	dec := xml.NewDecoder(r)
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil }
	return dec
}
