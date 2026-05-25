// composite.go layers a multi-signal quality scorer on top of
// the existing heuristic ScoreResponse. The original Scorer +
// QualityScore stay untouched so existing callers (proxy,
// stream, eval, ab) keep working. The new CompositeScorer
// (plus the four sub-scorers below) is the recommended default
// going forward; callers opt in by constructing it explicitly.
//
// Design choices:
//
//   - Each sub-scorer returns a [0, 1] float. Combining via
//     weights yields a calibrated overall score even when
//     individual signals have different sensitivity.
//   - "Expected type" is supplied by the caller because Lens
//     usually knows whether it asked for code, prose, or a
//     short reply (different features have different shapes).
//   - All scorers handle empty input — they return 0 rather
//     than panicking. The proxy can't always tell upstream
//     it got an empty body and we don't want to crash the
//     scoring pipeline.

package quality

import (
	"regexp"
	"strings"
)

// ─── language-aware scoring ────────────────────────

// LanguageScorer rewards responses whose shape matches the
// language. Code responses get full marks for containing at
// least one fenced block; prose responses get marked for
// readable paragraph structure. Empty Language defaults to
// "prose" — the most common case.
type LanguageScorer struct {
	Language string
}

// Score returns a value in [0, 1]. The breakdown:
//   - code languages: presence of ```fences = 0.6 floor,
//     full credit for at least one fence + actual code lines.
//   - prose languages: rewards multi-sentence + paragraph
//     structure.
//   - unknown language: middle-of-the-road 0.5 baseline.
func (ls *LanguageScorer) Score(response string) float64 {
	r := strings.TrimSpace(response)
	if r == "" {
		return 0
	}
	lang := strings.ToLower(strings.TrimSpace(ls.Language))
	if isCodeLanguage(lang) {
		return scoreCodeResponse(r, lang)
	}
	if lang == "" || lang == "prose" || lang == "text" || lang == "english" {
		return scoreProseResponse(r)
	}
	// Unknown language — give partial credit so we don't punish
	// legitimate edge cases.
	return 0.5
}

var codeLanguages = map[string]bool{
	"go":         true,
	"golang":     true,
	"typescript": true,
	"ts":         true,
	"javascript": true,
	"js":         true,
	"python":     true,
	"py":         true,
	"rust":       true,
	"rs":         true,
	"java":       true,
	"kotlin":     true,
	"ruby":       true,
	"rb":         true,
	"swift":      true,
	"c":          true,
	"cpp":        true,
	"c++":        true,
	"csharp":     true,
	"c#":         true,
	"php":        true,
	"shell":      true,
	"bash":       true,
	"sql":        true,
}

func isCodeLanguage(lang string) bool {
	return codeLanguages[lang]
}

var fenceLineRe = regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*\\s*$")

func scoreCodeResponse(r, lang string) float64 {
	hasFence := strings.Contains(r, "```")
	if !hasFence {
		// Code responses without fences are usually low quality —
		// the model dumped raw text instead of structured code.
		// Bare snippets (5+ semicolons or curlies) still pass.
		if heuristicLooksLikeCode(r) {
			return 0.55
		}
		return 0.3
	}
	score := 0.6
	// Bonus for the fence carrying the right language tag.
	if lang != "" {
		alts := []string{lang}
		switch lang {
		case "typescript":
			alts = append(alts, "ts", "tsx")
		case "javascript":
			alts = append(alts, "js", "jsx")
		case "python":
			alts = append(alts, "py")
		case "rust":
			alts = append(alts, "rs")
		}
		for _, alt := range alts {
			if strings.Contains(r, "```"+alt) {
				score += 0.2
				break
			}
		}
	}
	// Bonus for at least one actual code line inside the fence.
	if fenceLineRe.MatchString(r) && strings.Count(r, "\n") >= 2 {
		score += 0.2
	}
	if score > 1 {
		score = 1
	}
	return score
}

func heuristicLooksLikeCode(r string) bool {
	symbols := strings.Count(r, ";") + strings.Count(r, "{") + strings.Count(r, "}")
	return symbols >= 4
}

func scoreProseResponse(r string) float64 {
	// Coarse readability heuristic. Real readability would need
	// syllable counts; the spec asks for "score higher for
	// readable prose" without specifying — multi-sentence and
	// multi-paragraph is a fair proxy.
	sentences := splitSentences(r)
	if len(sentences) == 0 {
		return 0
	}
	score := 0.5
	if len(sentences) >= 3 {
		score += 0.25
	}
	if strings.Contains(r, "\n\n") {
		score += 0.25
	}
	if score > 1 {
		score = 1
	}
	return score
}

// ─── length scoring ────────────────────────────────

// ScoreLength maps response length to a quality signal given
// the expected type. The thresholds match the spec; callers
// who pass an unknown type fall back to the "prose" curve.
//
// Empty response → 0 regardless of type.
func ScoreLength(response, expectedType string) float64 {
	n := len(strings.TrimSpace(response))
	if n == 0 {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(expectedType)) {
	case "short":
		// Short answers should be < 200 chars. Penalise verbose.
		switch {
		case n < 1:
			return 0
		case n <= 200:
			return 1
		case n <= 500:
			return 0.7
		case n <= 1500:
			return 0.4
		}
		return 0.2
	case "list":
		// Lists want 50–2000 chars typically.
		switch {
		case n < 50:
			return 0.4
		case n <= 2000:
			return 1
		case n <= 5000:
			return 0.7
		}
		return 0.4
	case "code":
		// Code wants substance but not chaos.
		switch {
		case n < 50:
			return 0.3
		case n <= 10000:
			return 1
		case n <= 20000:
			return 0.5
		}
		return 0.2
	default: // prose
		switch {
		case n < 50:
			return 0.3
		case n <= 10000:
			return 1
		case n <= 20000:
			return 0.6
		}
		return 0.3
	}
}

// ─── structure scoring ─────────────────────────────

var (
	headerRe = regexp.MustCompile(`(?m)^#{1,6}\s+\S`)
	bulletRe = regexp.MustCompile(`(?m)^\s*[-*]\s+\S`)
	numberRe = regexp.MustCompile(`(?m)^\s*\d+\.\s+\S`)
)

// ScoreStructure rewards responses that use markdown
// affordances appropriately. Each construct present adds to
// the score; the result is capped at 1 so a response can't
// game its way up by spamming every kind of marker.
func ScoreStructure(response string) float64 {
	r := strings.TrimSpace(response)
	if r == "" {
		return 0
	}
	score := 0.4 // baseline for any non-empty response
	if headerRe.MatchString(r) {
		score += 0.2
	}
	if bulletRe.MatchString(r) || numberRe.MatchString(r) {
		score += 0.2
	}
	if strings.Contains(r, "```") {
		score += 0.2
	}
	if score > 1 {
		score = 1
	}
	return score
}

// ─── coherence scoring ────────────────────────────

// deflectionPhrases are the canonical "model refused" / "model
// deflected" signals. Mid-confidence — present but not strong.
var deflectionPhrases = []string{
	"i cannot",
	"i can't",
	"i don't have access",
	"as an ai",
	"i'm unable to",
	"i'm not able to",
	"i apologize",
}

// ScoreCoherence checks for two failure modes:
//
//   - Hard cut-off: the response stops mid-sentence (no
//     terminator + > 100 chars). Returns 0.
//   - Deflection: the response contains a refusal phrase.
//     Returns 0.5.
//
// Otherwise 1.0.
func ScoreCoherence(response string) float64 {
	r := strings.TrimSpace(response)
	if r == "" {
		return 0
	}
	if len(r) > 100 && !endsWithTerminator(r) {
		return 0
	}
	lower := strings.ToLower(r)
	for _, p := range deflectionPhrases {
		if strings.Contains(lower, p) {
			return 0.5
		}
	}
	return 1
}

// ─── composite scorer ─────────────────────────────

// CompositeScorer combines the four signals via configurable
// weights. The default weights match the spec:
//
//	Length: 0.2, Structure: 0.3, Coherence: 0.4, Language: 0.1
//
// CompositeScorer is the recommended default going forward.
// The original Scorer.ScoreResponse stays for backwards
// compatibility with the proxy/stream/eval call sites.
type CompositeScorer struct {
	LengthWeight    float64
	StructureWeight float64
	CoherenceWeight float64
	LanguageWeight  float64
}

// DefaultCompositeScorer returns a scorer with the spec weights.
func DefaultCompositeScorer() *CompositeScorer {
	return &CompositeScorer{
		LengthWeight:    0.2,
		StructureWeight: 0.3,
		CoherenceWeight: 0.4,
		LanguageWeight:  0.1,
	}
}

// Score combines the sub-signals into a single [0, 1] number.
// `prompt` is currently unused but kept on the signature so
// future signals (prompt-relevance, etc.) don't break the API.
func (cs *CompositeScorer) Score(response, prompt, expectedType string) float64 {
	_ = prompt
	lang := ""
	if expectedType == "code" {
		// Best-effort: the caller may pass "code" with an
		// implicit language. Let LanguageScorer treat it as a
		// generic code language.
		lang = "code"
	}
	weights := cs.normalisedWeights()
	score :=
		weights.length*ScoreLength(response, expectedType) +
			weights.structure*ScoreStructure(response) +
			weights.coherence*ScoreCoherence(response) +
			weights.language*(&LanguageScorer{Language: lang}).Score(response)
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// ScoreWithLanguage is the richer entry point — same as Score
// but lets the caller pin the language for the language scorer
// (e.g. "go" / "typescript"). Useful for the proxy's per-call
// scoring once the model + provider are known.
func (cs *CompositeScorer) ScoreWithLanguage(response, prompt, expectedType, language string) float64 {
	_ = prompt
	weights := cs.normalisedWeights()
	score :=
		weights.length*ScoreLength(response, expectedType) +
			weights.structure*ScoreStructure(response) +
			weights.coherence*ScoreCoherence(response) +
			weights.language*(&LanguageScorer{Language: language}).Score(response)
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

type weightSet struct {
	length, structure, coherence, language float64
}

// normalisedWeights returns the weights normalised to sum to 1.
// Zeros get replaced with the spec defaults so a partially-
// initialised CompositeScorer still scores sensibly.
func (cs *CompositeScorer) normalisedWeights() weightSet {
	w := weightSet{cs.LengthWeight, cs.StructureWeight, cs.CoherenceWeight, cs.LanguageWeight}
	if w.length == 0 && w.structure == 0 && w.coherence == 0 && w.language == 0 {
		w = weightSet{0.2, 0.3, 0.4, 0.1}
	}
	sum := w.length + w.structure + w.coherence + w.language
	if sum <= 0 {
		return weightSet{0.2, 0.3, 0.4, 0.1}
	}
	w.length /= sum
	w.structure /= sum
	w.coherence /= sum
	w.language /= sum
	return w
}

// splitSentences trims and splits on common terminators.
func splitSentences(r string) []string {
	raw := sentenceSplitter.Split(r, -1)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ─── auto-retry helper ─────────────────────────────

// AutoRetryThreshold is the score below which the proxy retries
// the request once (when LENS_QUALITY_AUTO_RETRY is set). The
// proxy bumps temperature on retry so it doesn't repeat the
// exact same bad reply.
const AutoRetryThreshold = 0.4

// ShouldAutoRetry returns true when the score is below the
// threshold AND retries are enabled. Cap at one retry per
// request — `attempt` is the 0-based attempt count, so we
// only retry on attempt 0.
func ShouldAutoRetry(score float64, enabled bool, attempt int) bool {
	if !enabled {
		return false
	}
	if attempt >= 1 {
		return false
	}
	return score < AutoRetryThreshold
}
