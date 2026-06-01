package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// jsonConverter emits JSON inside a fenced ```json block. JSON is already a
// structured representation, so re-prosing it would lose fidelity; instead we
// validate it and pretty-print with a stable 2-space indent (json.Indent
// preserves key order and values exactly — it only reflows whitespace), which
// gives the model clean, consistent structure.
type jsonConverter struct{}

func (jsonConverter) Format() Format { return FormatJSON }

func (jsonConverter) Convert(_ context.Context, input []byte) (Result, error) {
	trimmed := bytes.TrimSpace(input)
	// Unmarshal-validate so the error carries the parse cause (offset etc.).
	var probe json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return Result{Format: FormatJSON}, fmt.Errorf("%w: %v", ErrConversionFailed, err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		return Result{Format: FormatJSON}, err
	}
	// fencedBlock guards against fence-break-out if the JSON contains ``` runs.
	return Result{Markdown: fencedBlock("json", buf.String()), Format: FormatJSON}, nil
}
