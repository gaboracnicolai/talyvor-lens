// Package distill converts documents into clean, token-efficient Markdown
// before they reach a model — a token-reduction STAGE in the gateway, not a
// general-purpose file converter. It is Go-native (no Python / MarkItDown
// sidecar) so the whole request path stays reviewable in one language.
//
// This package is the CONVERSION CORE only: format detection + one converter
// per supported format behind a small Converter seam. Adding a format later is
// a new registration, not a rewrite (mirrors the provider seam in
// internal/proxy). The semantic cache, request-path integration, savings
// attribution, and tiers are separate, later concerns and are deliberately NOT
// in this package yet.
//
// # Security
//
// Converters parse UNTRUSTED document bytes, so they are fail-safe by
// construction:
//   - bounded: input is capped (MaxInputBytes) and converters are written to
//     run in time linear in the input — no pathological backtracking;
//   - panic-proof: Distill/DistillAs recover any panic from a converter and
//     return it as an error (a malformed document must never crash the host);
//   - inert: converters never touch the network or filesystem, never resolve
//     external entities, and never execute embedded content. Active content
//     (HTML <script>/<style>, event-handler attributes, etc.) is stripped
//     during conversion.
//
// Stripping active content also makes conversion an incidental
// prompt-injection-surface reducer (a design synergy with the guardrails
// engine, exploited in a later PR). The security bar for THIS package is just
// the list above: safe parsing, no SSRF/file access, no code execution,
// bounded resources.
package distill

import (
	"context"
	"errors"
	"fmt"
)

// Format is a document format the engine can detect and convert.
type Format string

const (
	FormatHTML    Format = "html"
	FormatDOCX    Format = "docx"
	FormatXLSX    Format = "xlsx"
	FormatCSV     Format = "csv"
	FormatJSON    Format = "json"
	FormatXML     Format = "xml"
	FormatText    Format = "text" // plaintext / markdown passthrough
	FormatPDF     Format = "pdf"  // registered but deferred (see pdf.go)
	FormatUnknown Format = "unknown"
)

// MaxInputBytes bounds a single conversion. 10 MiB comfortably covers the
// documents that arrive inline in API requests while putting a hard ceiling on
// the work any one untrusted input can cause.
const MaxInputBytes = 10 << 20

// Result is the outcome of a conversion.
type Result struct {
	// Markdown is the clean, token-efficient rendering of the input.
	Markdown string
	// Format is the source format the input was converted from.
	Format Format
	// NeedsVision reports that the input carries no extractable text layer
	// (e.g. a scanned, image-only PDF) and should be routed to the vision
	// fallback. RESERVED: it is set by the PDF/vision converter, which lands in
	// a later PR; no converter in this package sets it yet.
	NeedsVision bool
	// Warnings holds non-fatal structural notes (e.g. a table with ragged
	// rows). Conversion still succeeds; these are advisory.
	Warnings []string
}

// Converter turns one document format into Markdown. Implementations must be
// fail-safe: return an error on malformed/unconvertible input, never panic
// (the package-level recover is a backstop, not a license), and stay within
// the caller's size/time bounds — honoring ctx cancellation in long loops.
type Converter interface {
	// Format reports the format this converter handles.
	Format() Format
	// Convert parses input and returns clean Markdown.
	Convert(ctx context.Context, input []byte) (Result, error)
}

// Sentinel errors. Callers can errors.Is against these to branch (e.g. route a
// FormatPDF/ErrPDFPending input elsewhere, or surface ErrTooLarge distinctly).
var (
	ErrEmptyInput        = errors.New("distill: empty input")
	ErrTooLarge          = errors.New("distill: input exceeds size limit")
	ErrUnsupportedFormat = errors.New("distill: unsupported or undetected format")
	ErrConversionFailed  = errors.New("distill: conversion failed")
	// ErrPDFPending is returned for PDF input: the format is recognized and
	// reserved in the seam, but real PDF text extraction (and the text-less /
	// needs-vision signal) lands in a dedicated later PR. It is NOT a broken
	// or empty conversion — it is an explicit "not yet supported" signal.
	ErrPDFPending = errors.New("distill: PDF support pending (separate PR)")
)

// registry maps a detected format to its converter. Populated by init via
// register(); adding a format is a single register() call (additive seam).
var registry = map[Format]Converter{}

func register(c Converter) { registry[c.Format()] = c }

func init() {
	register(textConverter{})
	register(htmlConverter{})
	register(csvConverter{})
	register(jsonConverter{})
	register(xmlConverter{})
	register(docxConverter{})
	register(xlsxConverter{})
	register(pdfConverter{})
}

// Distill detects the format of input and converts it to Markdown. When the
// gateway already knows the declared content type, prefer DistillAs to skip
// sniffing (content sniffing is best-effort; some formats — notably CSV vs
// plaintext — are ambiguous from bytes alone).
func Distill(ctx context.Context, input []byte) (Result, error) {
	return convert(ctx, input, nil)
}

// DistillAs converts input as the explicitly-supplied format (e.g. resolved
// from a request's Content-Type by the future request-path integration).
func DistillAs(ctx context.Context, input []byte, format Format) (Result, error) {
	return convert(ctx, input, &format)
}

// convert enforces the size bound, picks the converter (detecting the format
// when none is supplied), and runs the whole thing under a panic recover. The
// recover deliberately spans DETECTION too: DetectFormat parses an untrusted
// ZIP central directory, which is itself an attacker-reachable parse — a panic
// there must never escape to the host.
func convert(ctx context.Context, input []byte, format *Format) (res Result, err error) {
	// Cheap, panic-free guards first.
	if len(input) == 0 {
		return Result{Format: derefFormat(format)}, ErrEmptyInput
	}
	if len(input) > MaxInputBytes {
		return Result{Format: derefFormat(format)}, fmt.Errorf("%w: %d bytes > %d", ErrTooLarge, len(input), MaxInputBytes)
	}

	f := derefFormat(format)
	defer func() {
		if r := recover(); r != nil {
			res = Result{Format: f}
			err = fmt.Errorf("%w: recovered from panic during %s handling: %v", ErrConversionFailed, f, r)
		}
	}()

	if format == nil {
		f = DetectFormat(input) // inside recover: untrusted ZIP/XML sniffing
	}
	c, ok := registry[f]
	if !ok {
		return Result{Format: f}, ErrUnsupportedFormat
	}
	return c.Convert(ctx, input)
}

func derefFormat(f *Format) Format {
	if f == nil {
		return FormatUnknown
	}
	return *f
}
