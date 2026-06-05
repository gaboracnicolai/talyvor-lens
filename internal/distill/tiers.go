package distill

import (
	"fmt"
	"regexp"
	"strings"
)

// Tier selects conversion FIDELITY — the fidelity/savings tradeoff. More
// compression drops more detail; each tier documents exactly what it drops, so
// an over-aggressive choice is honest and traceable (the chosen tier is
// recorded in Result.Tier).
type Tier string

const (
	// TierFaithful preserves ALL structure: the converters' full Markdown,
	// byte-for-byte. The default. Drops nothing.
	TierFaithful Tier = "faithful"

	// TierStructured keeps document structure (headings, tables, lists, code,
	// emphasis) but drops detected DECORATIVE content: page-number lines and
	// frequently-repeated running header/footer lines. For structured-DATA
	// formats (CSV/JSON/XML/XLSX) there is no such boilerplate, so structured
	// == faithful for them — it only differs where there is decoration to
	// remove. Drops: page markers + lines repeated ≥3× (running heads/feet).
	TierStructured Tier = "structured"

	// TierOutline keeps ONLY the heading hierarchy (which doubles as a table of
	// contents) plus a one-line summary per table, and OMITS body text, lists,
	// and code. For "what is this about" use — lossy by design. A document with
	// no headings (CSV/JSON/…) collapses to its table summary / a terse note.
	TierOutline Tier = "outline"
)

// normalizeTier maps the zero value and any unknown tier to faithful — the
// safe, lossless default.
func normalizeTier(t Tier) Tier {
	switch t {
	case TierStructured, TierOutline:
		return t
	default:
		return TierFaithful
	}
}

// applyTier post-processes a FAITHFUL conversion Result for the given tier and
// records the tier on the Result. faithful is the identity (converter output
// unchanged — no regression). It is a no-op on an empty or NeedsVision Result
// (no Markdown to reduce).
func applyTier(res Result, tier Tier) Result {
	tier = normalizeTier(tier)
	res.Tier = tier
	// Capture the faithful-text token baseline BEFORE any reduction, so
	// binary-origin savings can be measured as the tier delta (not a phantom
	// raw-bytes figure). Set on every usable conversion; survives caching.
	if res.Markdown != "" && !res.NeedsVision {
		res.FaithfulTextTokens = estTokens(res.Markdown)
	}
	if res.Markdown == "" || res.NeedsVision || tier == TierFaithful {
		return res
	}
	switch tier {
	case TierStructured:
		res.Markdown = structuredMarkdown(res.Markdown)
	case TierOutline:
		res.Markdown = outlineMarkdown(res.Markdown)
	}
	return res
}

// pageMarkerRe matches lines that are page numbers / running page markers:
// "Page 3", "Page 3 of 12", "- 7 -", "— 7 —". Conservative on purpose — a bare
// number line is NOT dropped (could be meaningful content).
var pageMarkerRe = regexp.MustCompile(`(?i)^\s*(page\s+\d+(\s+of\s+\d+)?|[-–—]\s*\d+\s*[-–—])\s*$`)

var orderedItemRe = regexp.MustCompile(`^\d+\.\s`)

// isStructuralLine reports whether a (trimmed) line carries document structure
// that must be preserved — headings, table rows, list items, code fences,
// blockquotes, rules. Such lines are never treated as droppable decoration.
func isStructuralLine(t string) bool {
	switch {
	case strings.HasPrefix(t, "#"),
		strings.HasPrefix(t, "|"),
		strings.HasPrefix(t, "```"),
		strings.HasPrefix(t, ">"),
		strings.HasPrefix(t, "- "),
		strings.HasPrefix(t, "* "),
		t == "---":
		return true
	}
	return orderedItemRe.MatchString(t)
}

// structuredMarkdown drops decorative content from faithful Markdown: page
// markers and frequently-repeated non-structural lines (running headers/
// footers). Structure is preserved.
func structuredMarkdown(md string) string {
	lines := strings.Split(md, "\n")

	// Count non-blank, non-structural lines to find running heads/feet.
	freq := map[string]int{}
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t != "" && !isStructuralLine(t) {
			freq[t]++
		}
	}

	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if pageMarkerRe.MatchString(t) {
			continue
		}
		if t != "" && !isStructuralLine(t) && freq[t] >= 3 {
			continue // running header/footer
		}
		out = append(out, ln)
	}
	return normalizeText(strings.Join(out, "\n"))
}

// outlineMarkdown reduces faithful Markdown to its heading hierarchy + a
// one-line summary per table, omitting body text/lists/code.
func outlineMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var headings []string
	var tableSummaries []string
	inCode := false

	i := 0
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(t, "```"):
			inCode = !inCode
			i++
		case inCode:
			i++ // omit code body
		case strings.HasPrefix(t, "#"):
			headings = append(headings, lines[i])
			i++
		case strings.HasPrefix(t, "|"):
			// Consume the contiguous table block; summarize it.
			cols := strings.Count(t, "|") - 1
			if cols < 0 {
				cols = 0
			}
			rows := 0
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				rows++
				i++
			}
			dataRows := rows - 2 // exclude the header row + the "| --- |" separator
			if dataRows < 0 {
				dataRows = 0
			}
			tableSummaries = append(tableSummaries, fmt.Sprintf("_Table: %d rows × %d columns (body omitted in outline)._", dataRows, cols))
		default:
			i++ // omit body prose / lists
		}
	}

	var out []string
	out = append(out, headings...)
	if len(tableSummaries) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, tableSummaries...)
	}
	if len(out) == 0 {
		return "_(outline: no headings or tables; body omitted)_"
	}
	return normalizeText(strings.Join(out, "\n"))
}
