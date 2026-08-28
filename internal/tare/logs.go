package tare

// logs.go — Tare phase 1: log collapse, the third of the three reductions W6.1 names
// ("JSON dedup, tree-sitter AST trimming, log collapse").
//
// ⚠ IT IS LOSSLESS, AND THAT WAS A CHOICE THE DESIGN ALREADY MADE RATHER THAN ONE MADE HERE.
// reports/tare-design-v1.html annotates the router "log-collapse [lossless first]", lists log
// collapse among "reductions that cannot silently drop signal an enterprise agent needs", labels
// phase 1 "Lossless structural reduction", and defers "the aggressive lossy path" to phase 2. A run
// of identical lines carries exactly one fact — the line, and how many times it occurred — so the
// collapsed form loses nothing and ExpandLog puts the bytes back exactly.
//
// ⚠ THIS IS ALSO WHY THERE IS NO "MUST-KEEP FOR NUMBERS/PATHS/IDS" RULE IN THIS FILE, AND ITS
// ABSENCE IS DELIBERATE RATHER THAN AN OVERSIGHT. That clause belongs to the phase-2 ML text model
// (the design's sourcing table attaches it to "Text model ModernBERT dual-head · extractive", and
// §07 says "the model is extractive"); it is the mitigation for something that CHOOSES which tokens
// to discard. This reducer discards nothing, so every number, path and ID survives by construction
// — the same argument docs/tare-phase1a-measured.md already made for the JSON reducer's keep-rule.
// internal/tare/conformance_test.go carries the measurement.
//
// ── WHAT IT COLLAPSES, AND WHAT IT DELIBERATELY DOES NOT ────────────────────────────────────────
//
// ADJACENT identical lines only. Collapsing identical lines that are far apart would need their
// positions recorded to be reversible, which costs more than it saves and turns a one-pass scan
// into a bookkeeping problem. A run is what logs actually produce — a retry loop, a flapping
// health check, a stuck poller — and it is what an agent reading the log gains nothing from seeing
// four hundred times.
//
// ⚠ THERE IS NO MINIMUM RUN LENGTH CONSTANT, ON PURPOSE. "Collapse runs of 3 or more" would be a
// threshold nobody measured, and it would be wrong in both directions: a run of two 200-byte lines
// is worth collapsing and a run of five empty lines is not. The rule is arithmetic instead — a run
// is collapsed when its collapsed form is SMALLER, which derives the minimum length from the line
// itself. See worthCollapsing.

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
)

// RepeatMarkerPrefix and repeatMarkerSuffix bracket the count line that replaces a collapsed run.
// A marker line is exactly RepeatMarkerPrefix + decimal digits + repeatMarkerSuffix, and it means
// "the line above this one occurred N more times".
//
// ⚠ THE BRACKETS ARE NOT DECORATION — THEY ARE WHAT MAKES EXPANSION UNAMBIGUOUS. A marker built
// from characters that appear in real logs could not be told apart from content, so the expansion
// would sometimes invent lines that were never there. U+27E6/U+27E7 do not occur in any log this
// repo holds, and the reducer REFUSES outright on content that already contains the prefix
// (ReasonMarkerInInput) rather than gambling that it is not a marker.
const (
	RepeatMarkerPrefix = "⟦tare ×"
	repeatMarkerSuffix = "⟧"
)

// Refusal reasons specific to the log collapser.
const (
	ReasonNoRepeats = "no run of repeated lines worth collapsing"
	// ⚠ SEPARATE FROM ReasonNoRepeats BECAUSE THEY MEAN DIFFERENT THINGS TO WHOEVER READS THE
	// REFUSAL COUNTS. "This log has no repetition" is a fact about the traffic; "this log already
	// contains our marker" is a fact about US, and if it ever becomes common the marker is wrong.
	ReasonMarkerInInput = "content already contains the tare repeat marker"
)

// LogCollapse replaces runs of identical adjacent lines with one copy and a count.
type LogCollapse struct {
	observe func(Refusal)
}

func NewLogCollapse() *LogCollapse { return &LogCollapse{} }

func (l *LogCollapse) WithObserver(f func(Refusal)) *LogCollapse { l.observe = f; return l }

func (l *LogCollapse) refuse(content []byte, kind Kind, reason string) ([]byte, int, int, error) {
	if l.observe != nil {
		l.observe(Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := EstimateTokens(content)
	return content, t, t, nil
}

// marker renders the count line for a run with n hidden repeats.
func marker(n int) string {
	return RepeatMarkerPrefix + strconv.Itoa(n) + repeatMarkerSuffix
}

// worthCollapsing reports whether replacing `hidden` further copies of `line` with a single marker
// line makes the output smaller. Each dropped copy saves len(line)+1 bytes (the line and its
// newline); the marker costs its own length plus a newline.
//
// ⚠ STRICTLY SMALLER, NOT "NOT LARGER". A collapse that saves nothing still changes the bytes an
// agent reads, and buying a cosmetic change with a loss of fidelity — even reversible fidelity —
// is not a trade this layer should make silently.
func worthCollapsing(line string, hidden int) bool {
	if hidden <= 0 {
		return false
	}
	saved := hidden * (len(line) + 1)
	cost := len(marker(hidden)) + 1
	return saved > cost
}

// Reduce implements Reduction.
//
// ⚠ THE SPLIT AND REJOIN ARE BYTE-EXACT AND THAT IS LOAD-BEARING. Splitting on "\n" and rejoining
// with "\n" preserves a missing trailing newline, a trailing blank line, and CRLF endings (the
// "\r" simply rides along as the last byte of each line, and two CRLF lines are identical exactly
// when their content is). The round-trip is compared BYTE-FOR-BYTE by the conformance harness, so
// anything less than exact would fail there rather than pass quietly.
func (l *LogCollapse) Reduce(_ context.Context, content []byte, kind Kind) ([]byte, int, int, error) {
	if kind != KindLog {
		return l.refuse(content, kind, ReasonWrongKind)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return l.refuse(content, kind, ReasonEmpty)
	}
	if bytes.Contains(content, []byte(RepeatMarkerPrefix)) {
		return l.refuse(content, kind, ReasonMarkerInInput)
	}

	lines := bytes.Split(content, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	collapsed := false

	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && bytes.Equal(lines[j], lines[i]) {
			j++
		}
		run := j - i // total occurrences, including the one we keep
		hidden := run - 1
		if worthCollapsing(string(lines[i]), hidden) {
			out = append(out, lines[i], []byte(marker(hidden)))
			collapsed = true
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}

	if !collapsed {
		return l.refuse(content, kind, ReasonNoRepeats)
	}
	reduced := bytes.Join(out, []byte("\n"))
	// ⚠ CHECKED RATHER THAN ASSUMED. worthCollapsing is per-run and the whole-document result is
	// what gets billed, so the document-level property is asserted here too. They agree today; the
	// day they stop, this refuses instead of booking a negative saving.
	if len(reduced) >= len(content) {
		return l.refuse(content, kind, ReasonNotSmaller)
	}
	return reduced, EstimateTokens(content), EstimateTokens(reduced), nil
}

// ExpandLog is the inverse. It is what makes the losslessness demonstrable rather than asserted —
// the conformance harness runs it over every sample and compares the result to the input byte for
// byte.
//
// ⚠ IT ERRORS RATHER THAN GUESSING. A marker with no line above it, or a count that is not a
// positive integer, means the input was not produced by this reducer; inventing a plausible
// expansion there would manufacture log lines that never existed, which is the exact harm this
// whole package is built to avoid.
func ExpandLog(b []byte) ([]byte, error) {
	lines := bytes.Split(b, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for i, ln := range lines {
		s := string(ln)
		if !isMarker(s) {
			out = append(out, ln)
			continue
		}
		n, err := markerCount(s)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("tare: repeat marker at line %d has no line above it to repeat", i+1)
		}
		prev := out[len(out)-1]
		for k := 0; k < n; k++ {
			out = append(out, prev)
		}
	}
	return bytes.Join(out, []byte("\n")), nil
}

func isMarker(s string) bool {
	return len(s) > len(RepeatMarkerPrefix)+len(repeatMarkerSuffix) &&
		s[:len(RepeatMarkerPrefix)] == RepeatMarkerPrefix &&
		s[len(s)-len(repeatMarkerSuffix):] == repeatMarkerSuffix
}

func markerCount(s string) (int, error) {
	digits := s[len(RepeatMarkerPrefix) : len(s)-len(repeatMarkerSuffix)]
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("tare: repeat marker %q does not carry a positive count", s)
	}
	return n, nil
}
