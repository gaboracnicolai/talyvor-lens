package distill

import (
	"archive/zip"
	"bytes"
	"context"
	"strconv"
	"strings"
)

// xlsxConverter renders the first worksheet of an .xlsx (OOXML ZIP) as a pipe
// table, stdlib-only (archive/zip + encoding/xml). Cell values are resolved
// through the shared-strings table; cells are placed by their column reference
// (A1/C1/…) so sparse rows stay aligned. The first row is the header.
//
// Decompression of each part is capped (zip-bomb defense, see readZipPart).
type xlsxConverter struct{}

func (xlsxConverter) Format() Format { return FormatXLSX }

func (xlsxConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	zr, err := openZip(input)
	if err != nil {
		return Result{Format: FormatXLSX}, err
	}

	shared, err := readSharedStrings(zr)
	if err != nil {
		return Result{Format: FormatXLSX}, err
	}

	sheetName := firstWorksheetName(zr)
	if sheetName == "" {
		return Result{Format: FormatXLSX}, ErrConversionFailed
	}
	sheetData, err := readZipPart(zr, sheetName)
	if err != nil {
		return Result{Format: FormatXLSX}, err
	}

	var sheet struct {
		Rows []struct {
			Cells []struct {
				Ref  string `xml:"r,attr"`
				Type string `xml:"t,attr"`
				V    string `xml:"v"`
				IS   struct {
					Texts    []string `xml:"t"`
					RunTexts []string `xml:"r>t"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := newSafeXMLDecoder(bytes.NewReader(sheetData)).Decode(&sheet); err != nil {
		return Result{Format: FormatXLSX}, err
	}

	var grid [][]string
	for _, row := range sheet.Rows {
		if err := ctx.Err(); err != nil {
			return Result{Format: FormatXLSX}, err
		}
		var cells []string
		for _, c := range row.Cells {
			col := colIndex(c.Ref)
			val := resolveCell(c.Type, c.V, strings.Join(append(c.IS.Texts, c.IS.RunTexts...), ""), shared)
			if col < 0 {
				cells = append(cells, val) // no/odd ref → append in order
				continue
			}
			for len(cells) <= col {
				cells = append(cells, "")
			}
			cells[col] = val
		}
		grid = append(grid, cells)
	}
	// Drop leading fully-empty rows so the header is the first row with content.
	for len(grid) > 0 && allEmpty(grid[0]) {
		grid = grid[1:]
	}
	if len(grid) == 0 {
		return Result{Markdown: "", Format: FormatXLSX}, nil
	}
	return Result{Markdown: mdTable(grid[0], grid[1:]), Format: FormatXLSX}, nil
}

func readSharedStrings(zr *zip.Reader) ([]string, error) {
	for _, f := range zr.File {
		if f.Name != "xl/sharedStrings.xml" {
			continue
		}
		data, err := readZipPart(zr, f.Name)
		if err != nil {
			return nil, err
		}
		var sst struct {
			Items []struct {
				Texts    []string `xml:"t"`
				RunTexts []string `xml:"r>t"`
			} `xml:"si"`
		}
		if err := newSafeXMLDecoder(bytes.NewReader(data)).Decode(&sst); err != nil {
			return nil, err
		}
		out := make([]string, len(sst.Items))
		for i, it := range sst.Items {
			out[i] = strings.Join(append(it.Texts, it.RunTexts...), "")
		}
		return out, nil
	}
	return nil, nil // no shared strings table (all-inline/numeric sheet) is valid
}

func firstWorksheetName(zr *zip.Reader) string {
	// Prefer the conventional first sheet, else the lexically-first worksheet.
	best := ""
	for _, f := range zr.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			return f.Name
		}
		if strings.HasPrefix(f.Name, "xl/worksheets/") && strings.HasSuffix(f.Name, ".xml") {
			if best == "" || f.Name < best {
				best = f.Name
			}
		}
	}
	return best
}

func resolveCell(typ, v, inline string, shared []string) string {
	switch typ {
	case "s": // shared string: v is the index
		if idx, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && idx >= 0 && idx < len(shared) {
			return shared[idx]
		}
		return ""
	case "inlineStr":
		return inline
	default: // number, bool, date-serial, "str" formula result, etc. → literal
		return v
	}
}

// colIndex converts a cell ref ("A1", "AB12") to a 0-based column index, or -1
// if the ref is missing/odd.
func colIndex(ref string) int {
	letters := ref
	for i, r := range ref {
		if r >= '0' && r <= '9' {
			letters = ref[:i]
			break
		}
	}
	if letters == "" {
		return -1
	}
	col := 0
	for _, r := range letters {
		c := r | 0x20 // lower-case
		if c < 'a' || c > 'z' {
			return -1
		}
		col = col*26 + int(c-'a'+1)
	}
	return col - 1
}

func allEmpty(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
