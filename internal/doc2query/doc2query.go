// Package doc2query widens pooled recall by deriving, at write time, the questions a stored answer
// would answer — and embedding each as an additional match target for that ONE answer.
//
// ⚠ WHY RECALL IS THE WHOLE PROBLEM NOW. #392 put an entity gate in front of the pooled read, which
// broke the inversion: genuine rephrasings now sit above the dangerous pairs instead of below them.
// But the safe window it opened (0.807–0.849) serves 1 of 28 measured rephrasings. The pool is safe
// and nearly useless, and no threshold move fixes that — the rephrasings simply are not similar
// enough to each other. They are, however, similar to the ANSWER, which is the asymmetry this
// exploits.
//
// ⚠ THE ANSWER IS NEVER MODIFIED, PARAPHRASED, OR TRUNCATED. Variants are match targets and nothing
// else; a hit serves the contributor's exact bytes. Serving generated text would mean shipping
// prose no model produced for that question and the contributor never wrote — fluent, unauditable,
// and credited to them.
//
// ⚠ AND VARIANTS INHERIT THE ORIGINAL'S DISCRIMINATORS, NEVER THEIR OWN. This is the property the
// whole design rests on. A variant derived from a Pydantic v2 answer may legitimately omit the
// version in its own text ("how do I validate a field?"). If that variant carried the entities of
// its own text, it would become a match target with NO version constraint pointing at
// version-specific content — a hole that serves v2 prose to someone who never said v2, and one
// this package would have opened rather than found. Inheriting the original's discriminators
// confines doc2query to widening recall INSIDE an entity class, which is the only place it is safe.
package doc2query

import (
	"context"
	"strings"
)

// Variant is one derived question pointing back at the stored answer.
//
// It deliberately has NO discriminator field. The inheritance rule is not a value a caller could
// get wrong; it is enforced at the write site by copying the original's, and there is no way to
// express a variant carrying its own.
type Variant struct {
	Question  string
	Embedding []float32
}

// Deriver turns a stored answer into the questions it would answer.
type Deriver interface {
	Derive(ctx context.Context, answer string, n int) ([]string, error)
}

// Prompt is the instruction given to the deriving model.
//
// ⚠ IT ASKS FOR QUESTIONS THE ANSWER ANSWERS, NOT QUESTIONS THE ANSWER IS ABOUT. The difference is
// the whole value: "what is this text about" produces topic labels, which are already close to the
// original question and add nothing. What widens recall is the phrasings a DIFFERENT person would
// have typed to arrive here — the symptom report, the casual register, the vocabulary the original
// asker did not use.
const Prompt = `Below is an answer that was given to a technical question.

Write %d SHORT questions that this answer would correctly and completely answer.

Rules:
- Each question must be answerable IN FULL by the text below. If the answer only partly covers it, do not write it.
- Vary the phrasing widely: a formal question, a symptom report ("my build fails with..."), a casual ask, different vocabulary for the same concept.
- Do NOT copy sentences from the answer.
- One question per line, no numbering, no preamble.

ANSWER:
%s`

// ParseQuestions turns the model's reply into questions, discarding anything that does not look
// like one. A deriver that returns prose, numbering, or an apology must yield zero variants rather
// than pollute the pool with match targets nobody would ever type.
func ParseQuestions(raw string, max int) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		q := strings.TrimSpace(line)
		q = strings.TrimLeft(q, "-*0123456789.) \t")
		q = strings.TrimSpace(q)
		if len(q) < 8 || len(q) > 200 {
			continue
		}
		// A derived match target that is not a question is a statement lifted from the answer,
		// which would match the answer's own phrasing rather than a user's.
		if !strings.Contains(q, "?") && !looksLikeAsk(q) {
			continue
		}
		out = append(out, q)
		if len(out) >= max {
			break
		}
	}
	return out
}

func looksLikeAsk(q string) bool {
	l := strings.ToLower(q)
	for _, p := range []string{"how ", "what ", "why ", "when ", "which ", "where ", "can ", "is ", "does ", "do i "} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}
