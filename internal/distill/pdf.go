package distill

import "context"

// pdfConverter reserves the PDF slot in the converter seam but does NOT yet
// extract text. Real PDF support — choosing a pure-Go extraction library after
// evaluating its license, maintenance, and extraction quality, plus the
// text-less / scanned-PDF needs-vision signal (Result.NeedsVision) — lands in
// a dedicated later PR. Until then this returns a clear "pending" signal rather
// than an empty or faked conversion, so adding the real converter later is
// purely additive (replace this stub; the registration stays).
type pdfConverter struct{}

func (pdfConverter) Format() Format { return FormatPDF }

func (pdfConverter) Convert(_ context.Context, _ []byte) (Result, error) {
	return Result{Format: FormatPDF}, ErrPDFPending
}
