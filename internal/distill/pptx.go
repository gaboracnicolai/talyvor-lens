package distill

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// pptxConverter extracts the text of a .pptx slide deck (an OOXML ZIP) using only stdlib (B18.13): each
// slide, in presentation order, becomes a "## Slide N — <title>" section holding its text boxes'
// paragraphs and its tables (as pipe tables). Pictures, charts and speaker notes are not read — the
// text is what a model can use, and it is a small fraction of a deck's bytes, which is the saving.
//
// Every part is read through readZipPart, so decompression is capped exactly as for DOCX and XLSX.
type pptxConverter struct{}

func (pptxConverter) Format() Format { return FormatPPTX }

func (pptxConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	zr, err := openZip(input)
	if err != nil {
		return Result{Format: FormatPPTX}, err
	}
	slides := pptxSlideOrder(zr)
	if len(slides) == 0 {
		return Result{Format: FormatPPTX}, ErrConversionFailed
	}
	var sections []string
	for i, name := range slides {
		if err := ctx.Err(); err != nil {
			return Result{Format: FormatPPTX}, err
		}
		data, err := readZipPart(zr, name)
		if err != nil {
			return Result{Format: FormatPPTX}, err
		}
		title, blocks, err := pptxSlideText(data)
		if err != nil {
			return Result{Format: FormatPPTX}, err
		}
		head := fmt.Sprintf("## Slide %d", i+1)
		if title != "" {
			head += " — " + title
		}
		sections = append(sections, strings.Join(append([]string{head}, blocks...), "\n\n"))
	}
	return Result{Markdown: normalizeText(strings.Join(sections, "\n\n")), Format: FormatPPTX}, nil
}

var pptxSlidePart = regexp.MustCompile(`^ppt/slides/slide(\d+)\.xml$`)

// pptxSlideOrder is the deck's slide parts in presentation order: presentation.xml's slide list resolved
// through its relationships. A deck whose list cannot be read falls back to the slide files' numbers.
func pptxSlideOrder(zr *zip.Reader) []string {
	var byNumber []string
	for _, f := range zr.File {
		if pptxSlidePart.MatchString(f.Name) {
			byNumber = append(byNumber, f.Name)
		}
	}
	num := func(name string) int { n, _ := strconv.Atoi(pptxSlidePart.FindStringSubmatch(name)[1]); return n }
	sort.Slice(byNumber, func(i, j int) bool { return num(byNumber[i]) < num(byNumber[j]) })

	pres, err1 := readZipPart(zr, "ppt/presentation.xml")
	rels, err2 := readZipPart(zr, "ppt/_rels/presentation.xml.rels")
	if err1 != nil || err2 != nil {
		return byNumber
	}
	var p struct {
		IDs []struct {
			RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
		} `xml:"sldIdLst>sldId"`
	}
	var r struct {
		Rels []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if newSafeXMLDecoder(bytes.NewReader(pres)).Decode(&p) != nil || newSafeXMLDecoder(bytes.NewReader(rels)).Decode(&r) != nil {
		return byNumber
	}
	target := map[string]string{}
	for _, rel := range r.Rels {
		target[rel.ID] = path.Clean(path.Join("ppt", rel.Target))
	}
	var ordered []string
	for _, id := range p.IDs {
		if t, ok := target[id.RID]; ok && pptxSlidePart.MatchString(t) {
			ordered = append(ordered, t)
		}
	}
	if len(ordered) == 0 {
		return byNumber
	}
	return ordered
}

type aParagraph struct {
	Runs []struct {
		XMLName xml.Name
		Text    string `xml:"t"`
	} `xml:",any"` // a:r, a:fld and a:br in document order; only their a:t is read
}

func (p aParagraph) text() string {
	var b strings.Builder
	for _, r := range p.Runs {
		if r.XMLName.Local == "br" {
			b.WriteByte(' ')
		}
		b.WriteString(r.Text)
	}
	return strings.TrimSpace(collapseWS(b.String()))
}

// pptxSlideText is a slide's title (its title placeholder's text) and its other blocks in order.
func pptxSlideText(data []byte) (string, []string, error) {
	var title string
	var blocks []string
	dec := newSafeXMLDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return title, blocks, nil
		}
		if err != nil {
			return "", nil, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "sp":
			var sp struct {
				Ph *struct {
					Type string `xml:"type,attr"`
				} `xml:"nvSpPr>nvPr>ph"`
				Paras []aParagraph `xml:"txBody>p"`
			}
			if err := dec.DecodeElement(&sp, &se); err != nil {
				return "", nil, err
			}
			var lines []string
			for _, p := range sp.Paras {
				if t := p.text(); t != "" {
					lines = append(lines, t)
				}
			}
			if len(lines) == 0 {
				continue
			}
			if sp.Ph != nil && (sp.Ph.Type == "title" || sp.Ph.Type == "ctrTitle") && title == "" {
				title = strings.Join(lines, " ")
				continue
			}
			blocks = append(blocks, strings.Join(lines, "\n"))
		case "tbl":
			var t struct {
				Rows []struct {
					Cells []struct {
						Paras []aParagraph `xml:"txBody>p"`
					} `xml:"tc"`
				} `xml:"tr"`
			}
			if err := dec.DecodeElement(&t, &se); err != nil {
				return "", nil, err
			}
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
			if len(grid) > 0 {
				blocks = append(blocks, mdTable(grid[0], grid[1:]))
			}
		}
	}
}
