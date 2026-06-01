package distill

import "context"

// textConverter is the plaintext / Markdown passthrough. Markdown is already
// the target representation, and plain text needs no structural extraction —
// so this only normalizes whitespace (line endings, trailing spaces, excess
// blank lines). It's the safe fallback for anything detected as text.
type textConverter struct{}

func (textConverter) Format() Format { return FormatText }

func (textConverter) Convert(_ context.Context, input []byte) (Result, error) {
	return Result{Markdown: normalizeText(string(input)), Format: FormatText}, nil
}
