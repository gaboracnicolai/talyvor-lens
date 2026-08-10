// Package canonq is the MEASUREMENT SURFACE for W2.6's tier-2 proposal: rewrite every prompt into
// a canonical question with a cheap model, hash THAT, and match exactly — so that "B asks
// something equivalent, different wording, same intent" becomes an exact-key hit instead of a
// threshold judgement.
//
// ⚠ IT IS NOT WIRED INTO ANY SERVE PATH AND MUST NOT BE. W2.6 says measure, report two numbers,
// stop. `no_serve_path_test.go` in this package asserts that, from the product source, so the
// instruction survives the session that wrote it.
//
// ⚠ WHY THIS IS A DIFFERENT SHAPE OF RISK FROM A THRESHOLD, and the reason the item wants it:
// W2.1 measured that cosine similarity CANNOT deliver the promise — genuine rephrasings score
// 0.8681 while opposite-meaning pairs score 0.9770, so the ranking is inverted and no threshold
// separates them. Exact-matching a canonical form removes the threshold entirely. What replaces
// it is a question that can be measured rather than argued: does the normaliser ever collapse two
// questions with DIFFERENT answers?
//
// ⚠ AND THE FAILURE MODE THAT HAS NO ANALOGUE ON THE SIMILARITY PATH: the canonicaliser is an
// LLM, so it has a CONSTANT-OUTPUT mode. A refusal, an apology, or a generic placeholder is the
// same string for every prompt that provokes it — and under exact-key matching, identical strings
// are a HIT. One refusal shape shared by two danger prompts is not a missing measurement, it is a
// mass false serve, with no similarity score left to inspect afterwards. Parse exists to make
// that state unrepresentable: a reply that is not a question yields "", and Key("") is "" rather
// than the perfectly good hash of the empty string that every failure would otherwise share.
package canonq

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// Prompt is the fixed instruction. The measurement is a property of THIS string as much as of the
// model: a canonicaliser measured under one instruction says nothing about another, so the prompt
// is committed beside the numbers it produced.
//
// ⚠ THE TWO RULES THE DANGER CORPUS TURNS ON are the entity and direction rules. Every consumer
// danger pair differs in ONE entity (who, what, how much) and every engineering one in a version,
// identifier or resource kind; a canonicaliser that "simplifies" those away collapses precisely
// the pairs that must not collapse.
const Prompt = `Rewrite the question below into a single canonical question.

Rules:
- Keep EVERY entity exactly as given: who or what the question is about, quantities, ages, versions, error names, identifiers, product names and places.
- Keep the DIRECTION of any relationship: who does what to whom must not be swapped or dropped.
- Do not generalise. If the question is about a child, a cat, a toddler or a specific version, the canonical question is about that same thing.
- Use plain wording and a fixed form so that two questions with the same meaning become the same sentence.
- Output ONLY the canonical question, on one line. No preamble, no quotes, no explanation.

QUESTION:
%s`

// maxCanonical bounds a canonical QUESTION. It is a second, independent filter beside the
// interrogative test: a reply that is question-shaped but 300 characters long has started
// answering, and hashing prose keys the pool on a string nobody will ever type again.
const maxCanonical = 200

// refusalPrefixes are the constant-output shapes. Anchored at the START of the reply because a
// canonical question may legitimately contain the word "cannot" ("what cannot be pickled in
// Python") — matching anywhere would discard real questions and, worse, discard them
// SELECTIVELY on the safety-adjacent half of the corpus.
var refusalPrefixes = []string{
	"i'm sorry", "i am sorry", "sorry", "i cannot", "i can't", "i can not",
	"i'm unable", "i am unable", "unfortunately", "as an ai", "i'm not able", "i am not able",
}

// labelPrefixes are the preamble labels a small model emits despite being told not to. Only these
// exact labels are stripped: a general "drop everything before the first colon" rule would eat the
// first clause of a legitimate question.
var labelPrefixes = []string{
	"canonical question", "canonical form", "canonical", "question", "output", "result", "rewritten",
}

// interrogatives open a question. A reply that opens with none of them and carries no question
// mark is a statement, and a statement is an answer that escaped the instruction.
var interrogatives = []string{
	"what", "which", "who", "whom", "whose", "when", "where", "why", "how",
	"is", "are", "was", "were", "can", "could", "should", "shall", "would", "will",
	"do", "does", "did", "may", "must", "define", "explain", "list", "name", "describe",
}

// Parse turns the model's reply into a canonical question, or "" if the reply cannot be one.
//
// ⚠ "" IS THE ONLY SAFE FAILURE VALUE and every rejection path returns it. A caller that receives
// "" has no key, so the pair simply does not collapse; a caller that received the refusal text
// would have a key shared with every other refusal.
func Parse(raw string) string {
	line := firstNonEmptyLine(raw)
	if line == "" {
		return ""
	}
	line = strings.Trim(line, " \t\"'`")
	line = stripLabel(line)
	line = strings.Trim(line, " \t\"'`")
	if line == "" || len(line) > maxCanonical {
		return ""
	}
	low := strings.ToLower(line)
	for _, p := range refusalPrefixes {
		if strings.HasPrefix(low, p) {
			return ""
		}
	}
	if !looksLikeQuestion(low) {
		return ""
	}
	return line
}

func firstNonEmptyLine(raw string) string {
	for _, l := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			return s
		}
	}
	return ""
}

func stripLabel(line string) string {
	i := strings.Index(line, ":")
	if i <= 0 {
		return line
	}
	label := strings.ToLower(strings.TrimSpace(line[:i]))
	for _, p := range labelPrefixes {
		if label == p {
			return strings.TrimSpace(line[i+1:])
		}
	}
	return line
}

func looksLikeQuestion(low string) bool {
	if strings.Contains(low, "?") {
		return true
	}
	first := low
	if i := strings.IndexAny(low, " \t"); i > 0 {
		first = low[:i]
	}
	first = strings.Trim(first, ".,;:!'\"")
	for _, w := range interrogatives {
		if first == w {
			return true
		}
	}
	return false
}

// Fold is the ONLY deterministic step in the key, and it is typographic: lower case, whitespace
// collapsed, terminal punctuation dropped, and the two mu spellings unified.
//
// ⚠ IT RUNS ON THE MODEL'S OUTPUT AND NEVER ON THE USER'S PROMPT. W2.5 measured what folding the
// PROMPT costs: discriminator's extractors are capital-anchored (`[A-Z]{2,}`, `[A-Z][a-z]{2,}`,
// CamelCase), so lower-casing the prompt deletes 74 entity tokens across 22.7% of the corpus and
// takes ENGINEERING danger pairs through the gate from 0/44 to 3/44. The canonical form is a
// SEPARATE string; the raw prompt still reaches extractPrompt, exact.Key, pooledPromptKey and
// discriminator.Canon untouched, which is the whole reason this can be measured at all without
// re-opening that hole.
func Fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range strings.ToLower(s) {
		if r == 'µ' { // MICRO SIGN -> GREEK SMALL LETTER MU
			r = 'μ'
		}
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteRune(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "?!.,;: \t")
}

// Key is the exact-match key: the hex sha256 of the folded canonical form.
//
// ⚠ IT REFUSES THE EMPTY FORM. sha256("") is a valid hash, and if it were returned every prompt
// the canonicaliser failed on would carry the SAME key — the exact mass-collapse this package
// exists to prevent. "" in, "" out, and a caller with no key cannot pool.
func Key(s string) string {
	f := Fold(s)
	if f == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(f))
	return hex.EncodeToString(sum[:])
}
