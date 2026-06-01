// Package distillpreview serves the admin-only DISTILL preview endpoint
// (POST /v1/admin/distill/preview): a DRY RUN that converts an uploaded document
// to Markdown through the isolated subprocess and returns the result — with NO
// model call, NO token_events write, and NO spend.
//
// # Isolated-path-only
//
// Untrusted document bytes arriving over HTTP are converted ONLY through the
// Converter seam (satisfied in production by *distill.ProcessIsolator, the
// killable, memory-limited subprocess). This package NEVER calls
// distill.Distill / distill.DistillAs — the in-process conversion of untrusted
// bytes that the stage-3 resource-isolation requirement forbids. The only
// distill functions it uses besides the Converter interface are the PURE
// post-processing helpers distill.ApplyTier (operates on already-converted
// Markdown) and distill.ComputeSavings (len/4 math) — neither parses the
// untrusted document. That dependency shape is the structural guarantee.
//
// # Side-effect-free
//
// The package imports nothing that writes spend, token_events, the ledger, or
// the request path. A preview cannot mutate accounting; it only reads a document
// and returns its conversion.
package distillpreview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/distill"
)

// Converter is the narrow conversion seam. In production it is
// *distill.ProcessIsolator (the isolated subprocess); tests inject a fake. The
// handler depends ONLY on this — it never reaches the in-process distill path.
type Converter interface {
	Convert(ctx context.Context, input []byte, format distill.Format) (distill.Result, error)
}

// Authorizer reports whether the request carries admin credentials. Injected so
// this package need not import internal/auth and so tests can simulate admin /
// non-admin. Production wraps authManager.Authenticate(req).IsAdmin.
type Authorizer func(r *http.Request) bool

// Handler serves the preview endpoint.
type Handler struct {
	Converter Converter
	IsAdmin   Authorizer
	// MaxBytes caps the request body. Zero uses distill.MaxInputBytes (10 MiB),
	// the same ceiling the converters enforce.
	MaxBytes int64
}

type savingsView struct {
	InputBytes           int `json:"input_bytes"`
	OutputBytes          int `json:"output_bytes"`
	InputTokensRaw       int `json:"input_tokens_raw"`
	InputTokensDistilled int `json:"input_tokens_distilled"`
	TokensSaved          int `json:"tokens_saved"`
}

type response struct {
	Markdown    string      `json:"markdown"`
	Format      string      `json:"format"`
	NeedsVision bool        `json:"needs_vision"`
	Tier        string      `json:"tier"`
	Warnings    []string    `json:"warnings,omitempty"`
	Savings     savingsView `json:"savings"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Admin gate first — fail closed if no authorizer is wired. A rejected
	// caller never reaches conversion (no work spent on the unauthorized).
	if h.IsAdmin == nil || !h.IsAdmin(r) {
		writeErr(w, http.StatusForbidden, "admin credentials required")
		return
	}

	// The caller DECLARES the media type; we do not sniff untrusted bytes
	// in-process. The worker uses an explicit format (DistillAs), so an
	// undeclared/unsupported type is a 400 rather than a guess.
	format, ok := formatFromContentType(r.Header.Get("Content-Type"))
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"unsupported or missing Content-Type; declare the document media type (e.g. application/pdf, text/html, text/csv)")
		return
	}

	maxBytes := h.MaxBytes
	if maxBytes <= 0 {
		maxBytes = distill.MaxInputBytes
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "document exceeds the size limit")
		return
	}
	if len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "empty document")
		return
	}

	// ISOLATED conversion ONLY — the killable subprocess, never in-process.
	res, err := h.Converter.Convert(r.Context(), body, format)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "conversion failed: "+err.Error())
		return
	}

	// Parent-side tier (default faithful = identity); pure post-processing of
	// the converted Markdown. Then measure savings on the len/4 basis.
	res = distill.ApplyTier(res, distill.Tier(strings.TrimSpace(r.URL.Query().Get("tier"))))
	sav := distill.ComputeSavings(body, res)

	writeJSON(w, http.StatusOK, response{
		Markdown:    res.Markdown,
		Format:      string(res.Format),
		NeedsVision: res.NeedsVision,
		Tier:        string(res.Tier),
		Warnings:    res.Warnings,
		Savings: savingsView{
			InputBytes:           sav.InputBytes,
			OutputBytes:          sav.OutputBytes,
			InputTokensRaw:       sav.InputTokensRaw,
			InputTokensDistilled: sav.InputTokensDistilled,
			TokensSaved:          sav.TokensSaved,
		},
	})
}

// formatFromContentType maps a declared media type to a distill.Format. Unknown
// or empty types return ok=false (the handler answers 400). Parameters such as
// "; charset=utf-8" are ignored.
func formatFromContentType(ct string) (distill.Format, bool) {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "application/pdf":
		return distill.FormatPDF, true
	case "text/html", "application/xhtml+xml":
		return distill.FormatHTML, true
	case "text/csv":
		return distill.FormatCSV, true
	case "application/json":
		return distill.FormatJSON, true
	case "text/xml", "application/xml":
		return distill.FormatXML, true
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return distill.FormatDOCX, true
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return distill.FormatXLSX, true
	case "text/plain", "text/markdown":
		return distill.FormatText, true
	}
	return distill.FormatUnknown, false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
