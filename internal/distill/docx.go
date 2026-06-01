package distill

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// maxZipEntries caps the number of members in an OOXML archive. A real
// .docx/.xlsx has tens to low-hundreds of parts; this is generous headroom
// while refusing archives whose entry count alone is a resource-exhaustion
// vector. (Per-part decompression is separately capped in readZipPart, and the
// whole input is already bounded by MaxInputBytes.)
const maxZipEntries = 4096

// openZip opens an in-memory ZIP with an entry-count guard. Used by every
// OOXML path (detect + docx + xlsx) so the bound is enforced uniformly.
func openZip(input []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(input), int64(len(input)))
	if err != nil {
		return nil, err
	}
	if len(zr.File) > maxZipEntries {
		return nil, fmt.Errorf("%w: archive has %d entries (> %d)", ErrTooLarge, len(zr.File), maxZipEntries)
	}
	return zr, nil
}

// docxConverter extracts text + structure from a .docx (an OOXML ZIP) using
// only stdlib: archive/zip to open the package and encoding/xml to walk
// word/document.xml. It surfaces headings (from paragraph styles), paragraphs,
// list items (paragraphs with numbering), and tables (as pipe tables). Run-
// level formatting (bold/italic) is intentionally dropped — text + block
// structure is what helps the model and keeps this bounded.
//
// Decompression is capped at MaxInputBytes (zip-bomb defense): a small archive
// must not expand into unbounded memory.
type docxConverter struct{}

func (docxConverter) Format() Format { return FormatDOCX }

func (docxConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	zr, err := openZip(input)
	if err != nil {
		return Result{Format: FormatDOCX}, err
	}
	data, err := readZipPart(zr, "word/document.xml")
	if err != nil {
		return Result{Format: FormatDOCX}, err
	}

	var blocks []string
	prevList := false // merge consecutive list-item paragraphs into one block
	dec := newSafeXMLDecoder(bytes.NewReader(data))
	inBody := false
	for {
		if err := ctx.Err(); err != nil {
			return Result{Format: FormatDOCX}, err
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Result{Format: FormatDOCX}, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "body":
			inBody = true
		case "p":
			if !inBody {
				continue
			}
			var p wParagraph
			if err := dec.DecodeElement(&p, &se); err != nil {
				return Result{Format: FormatDOCX}, err
			}
			b := p.markdown()
			if b == "" {
				continue
			}
			isList := p.PPr.NumPr != nil && headingLevel(p.PPr.PStyle.Val) == 0
			if isList && prevList && len(blocks) > 0 {
				blocks[len(blocks)-1] += "\n" + b
			} else {
				blocks = append(blocks, b)
			}
			prevList = isList
		case "tbl":
			if !inBody {
				continue
			}
			var t wTable
			if err := dec.DecodeElement(&t, &se); err != nil {
				return Result{Format: FormatDOCX}, err
			}
			if b := t.markdown(); b != "" {
				blocks = append(blocks, b)
			}
			prevList = false
		}
	}

	return Result{Markdown: normalizeText(strings.Join(blocks, "\n\n")), Format: FormatDOCX}, nil
}

// wParagraph mirrors the parts of <w:p> we use. Namespace prefixes are matched
// by local name (encoding/xml default), which is unambiguous within DOCX.
type wParagraph struct {
	PPr struct {
		PStyle struct {
			Val string `xml:"val,attr"`
		} `xml:"pStyle"`
		NumPr *struct{} `xml:"numPr"`
	} `xml:"pPr"`
	Runs []struct {
		Texts []string   `xml:"t"`
		Tabs  []struct{} `xml:"tab"`
	} `xml:"r"`
}

func (p wParagraph) text() string {
	var b strings.Builder
	for _, r := range p.Runs {
		for _, t := range r.Texts {
			b.WriteString(t)
		}
		for range r.Tabs {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(collapseWS(b.String()))
}

func (p wParagraph) markdown() string {
	txt := p.text()
	if txt == "" {
		return ""
	}
	if lvl := headingLevel(p.PPr.PStyle.Val); lvl > 0 {
		return strings.Repeat("#", lvl) + " " + txt
	}
	if p.PPr.NumPr != nil {
		return "- " + txt
	}
	return txt
}

type wTable struct {
	Rows []struct {
		Cells []struct {
			Paras []wParagraph `xml:"p"`
		} `xml:"tc"`
	} `xml:"tr"`
}

func (t wTable) markdown() string {
	var grid [][]string
	for _, row := range t.Rows {
		var cells []string
		for _, c := range row.Cells {
			var parts []string
			for _, p := range c.Paras {
				if s := p.text(); s != "" {
					parts = append(parts, s)
				}
			}
			cells = append(cells, strings.Join(parts, " "))
		}
		if len(cells) > 0 {
			grid = append(grid, cells)
		}
	}
	if len(grid) == 0 {
		return ""
	}
	return mdTable(grid[0], grid[1:])
}

// headingLevel maps a Word paragraph style id to a Markdown heading level, or
// 0 if it's not a heading. "Heading1".."Heading6" → 1..6; "Title" → 1.
func headingLevel(style string) int {
	s := strings.ToLower(strings.TrimSpace(style))
	switch s {
	case "title":
		return 1
	}
	if strings.HasPrefix(s, "heading") {
		n := strings.TrimSpace(strings.TrimPrefix(s, "heading"))
		switch n {
		case "1":
			return 1
		case "2":
			return 2
		case "3":
			return 3
		case "4":
			return 4
		case "5":
			return 5
		case "6":
			return 6
		}
	}
	return 0
}

// readZipPart reads a named part, capping decompressed size at MaxInputBytes.
func readZipPart(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		// +1 so we can detect "exactly at the cap means likely truncated/bomb".
		data, err := io.ReadAll(io.LimitReader(rc, MaxInputBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > MaxInputBytes {
			return nil, ErrTooLarge
		}
		return data, nil
	}
	return nil, ErrConversionFailed
}
