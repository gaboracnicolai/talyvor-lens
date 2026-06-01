package distill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
)

// TestConverterVersion_OutputFingerprint is a TRIPWIRE linking converter OUTPUT
// to ConverterVersion. It hashes the converters' results over a FIXED inline
// corpus (decoupled from the golden testdata so it trips only on converter
// changes, not fixture edits). If any converter's output changes, this
// fingerprint changes and the test fails — forcing a conscious decision:
//
//  1. bump distill.ConverterVersion (the conversion cache keys on it; without a
//     bump it would serve OLD-converter Markdown for the full cache TTL), and
//  2. update wantFingerprint below to the new value.
//
// This removes the one real hazard the cache review found: a forgotten version
// bump silently serving stale Markdown.
func TestConverterVersion_OutputFingerprint(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		format Format
		input  []byte
	}{
		{"html", FormatHTML, []byte(`<h1>FP</h1><p>a <b>b</b> <a href="https://x.com">c</a></p><ul><li>x</li><li>y</li></ul>`)},
		{"csv", FormatCSV, []byte("a,b\n1,2\n3,4")},
		{"json", FormatJSON, []byte(`{"k":"v","n":1,"arr":[1,2]}`)},
		{"xml", FormatXML, []byte(`<r><a>1</a><b>2</b></r>`)},
		{"text", FormatText, []byte("plain\n\n\ntext")},
		{"docx", FormatDOCX, buildDOCX()},
		{"xlsx", FormatXLSX, buildXLSX()},
		{"pdf-text", FormatPDF, buildPDF("fingerprint one", "fingerprint two")},
		{"pdf-textless", FormatPDF, buildPDF()},
	}
	h := sha256.New()
	for _, c := range cases {
		res, err := DistillAs(ctx, c.input, c.format)
		// Fold the full result shape (+err presence) in, so ANY behavior change trips.
		fmt.Fprintf(h, "%s|%s|%q|%v|err=%v\n", c.name, res.Format, res.Markdown, res.NeedsVision, err != nil)
	}
	got := hex.EncodeToString(h.Sum(nil))

	// Pinned fingerprint for ConverterVersion "1". Bump both together (see above).
	const wantFingerprint = "20f0a811a8172bd4d96c5998daae36dbe88e2c6ed2a94a5622a79cbbba44966d"
	if got != wantFingerprint {
		t.Fatalf("converter output fingerprint changed (current ConverterVersion=%q).\n  got:  %s\n  want: %s\n\nIf this is an INTENTIONAL converter-output change, you MUST:\n  (1) bump distill.ConverterVersion so the conversion cache invalidates stale Markdown, and\n  (2) set wantFingerprint to the got value.\nIf unintentional, a converter regressed — investigate before pinning.", ConverterVersion, got, wantFingerprint)
	}
}
