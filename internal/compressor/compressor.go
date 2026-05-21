package compressor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Compressor struct{}

type CompressionResult struct {
	OriginalPrompt    string
	CompressedPrompt  string
	OriginalTokens    int
	CompressedTokens  int
	SavingsPct        float64
	TechniquesApplied []string
}

func New() *Compressor { return &Compressor{} }

// codeBlockRE matches a fenced code block including both ``` fences.
// (?s) lets . match newlines so the body of the block is captured.
var codeBlockRE = regexp.MustCompile("(?s)```.*?```")

var (
	spaceRunRE   = regexp.MustCompile(`[ \t]+`)
	newlineRunRE = regexp.MustCompile(`\n{3,}`)

	redundantPhraseRE = buildAlternationRE([]string{
		"please ", "kindly ", "could you please ",
		"I would like you to ", "I want you to ",
		"can you please ", "would you mind ",
	})

	commonPatterns = buildPatternReplacements([]struct{ from, to string }{
		{"in order to", "to"},
		{"as well as", "and"},
		{"due to the fact that", "because"},
		{"at this point in time", "now"},
		{"in the event that", "if"},
		{"for the purpose of", "for"},
		{"with regard to", "regarding"},
		{"in spite of the fact that", "although"},
	})
)

// buildAlternationRE compiles a single case-insensitive alternation matching
// any of the given literals. Sorted longest-first so a literal that is a
// prefix of another (e.g. "please " vs "could you please ") doesn't shadow it.
func buildAlternationRE(phrases []string) *regexp.Regexp {
	sorted := append([]string(nil), phrases...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	quoted := make([]string, len(sorted))
	for i, p := range sorted {
		quoted[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(quoted, "|") + `)`)
}

type compiledPattern struct {
	re *regexp.Regexp
	to string
}

func buildPatternReplacements(raw []struct{ from, to string }) []compiledPattern {
	sorted := append([]struct{ from, to string }(nil), raw...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].from) > len(sorted[j].from) })
	out := make([]compiledPattern, len(sorted))
	for i, p := range sorted {
		out[i] = compiledPattern{
			re: regexp.MustCompile(`(?i)` + regexp.QuoteMeta(p.from)),
			to: p.to,
		}
	}
	return out
}

func (c *Compressor) Compress(_ context.Context, prompt string) CompressionResult {
	original := prompt

	// Extract code blocks so the text techniques don't touch them.
	type codeBlock struct {
		placeholder string
		original    string
		compressed  string
	}
	var blocks []codeBlock

	withPlaceholders := codeBlockRE.ReplaceAllStringFunc(prompt, func(match string) string {
		ph := fmt.Sprintf("\x00CODE%d\x00", len(blocks))
		blocks = append(blocks, codeBlock{placeholder: ph, original: match})
		return ph
	})

	// Apply text techniques to non-code text.
	whitespaceChanged := false
	redundantChanged := false
	patternsChanged := false

	text := withPlaceholders
	if next := dedupWhitespace(text); next != text {
		whitespaceChanged = true
		text = next
	}
	if next := redundantPhraseRE.ReplaceAllString(text, ""); next != text {
		redundantChanged = true
		text = next
	}
	for _, p := range commonPatterns {
		if next := p.re.ReplaceAllString(text, p.to); next != text {
			patternsChanged = true
			text = next
		}
	}

	// Compress each extracted code block, then splice back in.
	codeBlocksChanged := false
	for i := range blocks {
		blocks[i].compressed = compressCodeBlock(blocks[i].original)
		if blocks[i].compressed != blocks[i].original {
			codeBlocksChanged = true
		}
		text = strings.Replace(text, blocks[i].placeholder, blocks[i].compressed, 1)
	}

	// Final trim attributes to the "whitespace" technique per the spec.
	if trimmed := strings.TrimSpace(text); trimmed != text {
		whitespaceChanged = true
		text = trimmed
	}

	var techniques []string
	if whitespaceChanged {
		techniques = append(techniques, "whitespace")
	}
	if redundantChanged {
		techniques = append(techniques, "redundant_phrases")
	}
	if patternsChanged {
		techniques = append(techniques, "common_patterns")
	}
	if codeBlocksChanged {
		techniques = append(techniques, "code_blocks")
	}

	originalTokens := len(original) / 4
	compressedTokens := len(text) / 4
	var savingsPct float64
	if originalTokens > 0 {
		savingsPct = (1 - float64(compressedTokens)/float64(originalTokens)) * 100
	}

	return CompressionResult{
		OriginalPrompt:    original,
		CompressedPrompt:  text,
		OriginalTokens:    originalTokens,
		CompressedTokens:  compressedTokens,
		SavingsPct:        savingsPct,
		TechniquesApplied: techniques,
	}
}

func dedupWhitespace(text string) string {
	text = spaceRunRE.ReplaceAllString(text, " ")
	text = newlineRunRE.ReplaceAllString(text, "\n\n")
	return text
}

// compressCodeBlock removes blank lines and trailing spaces inside a fenced
// code block. The opening and closing ``` fences are preserved verbatim
// (only trailing whitespace on the fence lines is trimmed).
func compressCodeBlock(block string) string {
	lines := strings.Split(block, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		// Skip blank lines strictly inside the fenced block.
		if i > 0 && i < len(lines)-1 && trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}
