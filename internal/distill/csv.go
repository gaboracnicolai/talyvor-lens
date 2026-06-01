package distill

import (
	"context"
	"encoding/csv"
	"strings"
)

// csvConverter renders CSV as a GitHub pipe table — the structure an LLM reads
// most reliably. The first row is treated as the header. Ragged rows (variable
// field counts) are tolerated (FieldsPerRecord = -1) and noted as a warning.
type csvConverter struct{}

func (csvConverter) Format() Format { return FormatCSV }

func (csvConverter) Convert(ctx context.Context, input []byte) (Result, error) {
	r := csv.NewReader(strings.NewReader(string(input)))
	r.FieldsPerRecord = -1 // tolerate ragged rows rather than erroring
	r.LazyQuotes = true

	records, err := r.ReadAll()
	if err != nil {
		return Result{Format: FormatCSV}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{Format: FormatCSV}, err
	}
	if len(records) == 0 {
		return Result{Markdown: "", Format: FormatCSV}, nil
	}

	var warnings []string
	header := records[0]
	rows := records[1:]
	for _, row := range rows {
		if len(row) != len(header) {
			warnings = append(warnings, "csv: ragged rows (inconsistent field counts) — padded to the widest row")
			break
		}
	}

	return Result{
		Markdown: mdTable(header, rows),
		Format:   FormatCSV,
		Warnings: warnings,
	}, nil
}
