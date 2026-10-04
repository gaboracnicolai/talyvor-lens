package kompress

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/talyvor/lens/internal/tare"
)

// Where the weights live and what they are. The Dockerfile's `kompress` stage fetches both files at
// the pinned revision and checks their sha256 before they reach the image.
const (
	Name          = "kompress-small"
	DefaultDir    = "/models/kompress-small"
	WeightsFile   = "model.safetensors"
	TokenizerFile = "tokenizer.json"
)

const (
	// minWords: below this much prose a model pass costs more than it can save.
	minWords = 30
	// maxSeq is the most tokens one forward pass sees, [CLS] and [SEP] included.
	maxSeq = 512
	// tokenBudget caps the tokens the model reads per request, so one huge message cannot hold a
	// request for seconds (measured: ~0.33 s per 512 tokens on 10 cores, ~0.8 s on 2). Prose past
	// the budget goes upstream unchanged.
	tokenBudget = 2048
	// maxConcurrent model passes in this process. A pass uses every CPU; a request that finds both
	// slots taken is refused (sent unchanged) rather than queued behind them.
	maxConcurrent = 2
)

// classifier says, per word, whether the model keeps it, and what a word costs it in tokens.
type classifier interface {
	keep(ctx context.Context, words []string) ([]bool, error)
	tokens(word string) int
}

// Compressor is the shared, lazily-loaded model. The 279 MB of weights are read on the first
// request that needs them — a lens whose workspaces never opt in never loads them.
type Compressor struct {
	dir  string
	once sync.Once
	cls  classifier
	err  error
	sem  chan struct{}
}

// NewCompressor returns a Compressor over the weights in dir. Nothing is read until first use.
func NewCompressor(dir string) *Compressor {
	return &Compressor{dir: dir, sem: make(chan struct{}, maxConcurrent)}
}

// newWithClassifier is the test seam: a Compressor whose model is already "loaded".
func newWithClassifier(c classifier) *Compressor {
	x := &Compressor{cls: c, sem: make(chan struct{}, maxConcurrent)}
	x.once.Do(func() {})
	return x
}

func (c *Compressor) load() (classifier, error) {
	c.once.Do(func() {
		tok, err := LoadTokenizer(filepath.Join(c.dir, TokenizerFile))
		if err == nil {
			var m *Model
			if m, err = LoadModel(filepath.Join(c.dir, WeightsFile)); err == nil {
				c.cls = &modelClassifier{m: m, tok: tok}
			}
		}
		if err != nil {
			c.err = err
			slog.Warn("tare: phase 2a model unavailable; prose goes upstream unchanged", "model", Name, "dir", c.dir, "error", err)
		} else {
			slog.Info("tare: phase 2a model loaded", "model", Name, "dir", c.dir)
		}
	})
	return c.cls, c.err
}

// Reduction returns the phase 2a reducer, reporting each refusal to observe (nil is fine).
func (c *Compressor) Reduction(observe func(tare.Refusal)) tare.Reduction {
	return &reducer{c: c, observe: observe}
}

type reducer struct {
	c       *Compressor
	observe func(tare.Refusal)
}

func (r *reducer) refuse(content []byte, kind tare.Kind, reason string) ([]byte, int, int, error) {
	if r.observe != nil {
		r.observe(tare.Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := tare.EstimateTokens(content)
	return content, t, t, nil
}

// Reduce implements tare.Reduction for prose. It is LOSSY — the model drops words — so it runs only
// on what phase 1 refused, and never on code or JSON, which Tare reduces losslessly or not at all:
// content that parses as JSON or reads as code is refused whole, and inside a message, fenced and
// indented code blocks, tables, headings and code-like lines go upstream byte for byte. In the prose
// it does compress, words carrying numbers, paths, identifiers, inline code or a negation are always
// kept, so the facts and the polarity of a sentence survive.
func (r *reducer) Reduce(ctx context.Context, content []byte, kind tare.Kind) ([]byte, int, int, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return r.refuse(content, kind, tare.ReasonEmpty)
	}
	if kind != tare.KindProse && kind != tare.KindUnknown {
		return r.refuse(content, kind, tare.ReasonWrongKind)
	}
	if !utf8.Valid(content) || json.Valid(content) {
		return r.refuse(content, kind, tare.ReasonNotProse)
	}
	units := split(string(content))
	var prose, codeLines, lines int
	for _, u := range units {
		if u.prose {
			prose += len(strings.Fields(u.text))
		}
		if u.codeLike {
			codeLines++
		}
		if u.line {
			lines++
		}
	}
	if lines > 0 && codeLines*5 >= lines { // a fifth of the lines read as code: this is code, not prose
		return r.refuse(content, kind, tare.ReasonNotProse)
	}
	if prose < minWords {
		return r.refuse(content, kind, tare.ReasonTooLittleProse)
	}
	cls, err := r.c.load()
	if err != nil {
		return r.refuse(content, kind, tare.ReasonModelUnavailable)
	}
	select {
	case r.c.sem <- struct{}{}:
		defer func() { <-r.c.sem }()
	default:
		return r.refuse(content, kind, tare.ReasonModelBusy)
	}

	var out strings.Builder
	budget := tokenBudget
	for _, u := range units {
		if !u.prose || budget <= 0 {
			out.WriteString(u.text)
			continue
		}
		words := strings.Fields(u.body)
		keep, used, err := keepWords(ctx, cls, words, budget)
		if err != nil {
			return content, 0, 0, err
		}
		budget -= used
		out.WriteString(u.lead)
		first := true
		for i, w := range words {
			if keep[i] {
				if !first {
					out.WriteByte(' ')
				}
				out.WriteString(w)
				first = false
			}
		}
		out.WriteString(u.trail)
	}
	reduced := []byte(out.String())
	if len(reduced) >= len(content) {
		return r.refuse(content, kind, tare.ReasonNotSmaller)
	}
	return reduced, tare.EstimateTokens(content), tare.EstimateTokens(reduced), nil
}

// keepWords decides each word: the model's verdict, overruled to KEEP by mustKeep. used is the
// number of model tokens spent; if the budget runs out mid-unit, the rest of the unit is kept.
func keepWords(ctx context.Context, cls classifier, words []string, budget int) ([]bool, int, error) {
	keep := make([]bool, len(words))
	inCode := false
	lastNum := -1
	for i, w := range words {
		ticks := strings.Count(w, "`")
		keep[i] = inCode || ticks > 0 || mustKeep(w)
		if ticks%2 == 1 {
			inCode = !inCode
		}
		// "41 of the 57", "between 3 and 5": up to two words joining two numbers carry the relation
		// between them, and without them the numbers read as one.
		if strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			if lastNum >= 0 && i-lastNum <= 3 {
				for j := lastNum + 1; j < i; j++ {
					keep[j] = true
				}
			}
			lastNum = i
		}
	}
	used := 0
	for i, w := range words {
		if used += cls.tokens(w); used > budget {
			for j := i; j < len(keep); j++ {
				keep[j] = true
			}
			words, used = words[:i], budget
			break
		}
	}
	verdict, err := cls.keep(ctx, words)
	if err != nil {
		return nil, 0, err
	}
	for i, k := range verdict {
		keep[i] = keep[i] || k
	}
	return keep, used, nil
}

var negations = map[string]bool{
	"no": true, "not": true, "never": true, "none": true, "nothing": true, "nobody": true, "nowhere": true,
	"neither": true, "nor": true, "cannot": true, "without": true, "unless": true, "except": true,
}

// mustKeep is the rule the design calls "hardcoded must-keep for numbers/paths/IDs", plus negations:
// dropping "not" keeps every fact and inverts the sentence.
func mustKeep(w string) bool {
	core := strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) && r != '/' && r != '_' && r != '#' && r != '@' })
	lower := strings.ToLower(core)
	if negations[lower] || strings.HasSuffix(lower, "n't") || strings.HasSuffix(lower, "n’t") {
		return true
	}
	upper, letters := 0, 0
	for i, r := range core {
		switch {
		case unicode.IsDigit(r):
			return true
		case strings.ContainsRune(`/\_@=<>{}[]#$%|~^*+`, r):
			return true
		case r == '.' && i > 0 && i < len(core)-1: // file.go, v2.4, example.com
			return true
		case unicode.IsUpper(r):
			upper++
			if i > 0 { // camelCase, iPhone, HTTPServer
				return true
			}
		}
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 2 && upper == letters // API, UTC, TODO
}

// unit is a run of the message: prose the model may compress, or text that goes through verbatim.
type unit struct {
	text     string // the unit as it appeared, verbatim
	prose    bool
	lead     string // prose: what precedes the words (indent, list marker)
	body     string // prose: the words
	trail    string // prose: what follows them (the newline)
	codeLike bool   // an unfenced line that reads as code
	line     bool   // a non-blank unfenced line (for the code-line ratio)
}

var (
	fenceRe    = regexp.MustCompile("^\\s{0,3}(```|~~~)")
	listRe     = regexp.MustCompile(`^(\s*(?:[-*+]|\d{1,3}[.)])\s+)(.*)$`)
	codeLineRe = regexp.MustCompile(`(^\s*(//|/\*|\*/|#include|import |package |func |def |class |return |const |let |var |SELECT |INSERT |UPDATE |[{}]))|([{};]\s*$)|(\)\s*\{)|(=>)|(:=)`)
)

// split cuts content into lines and groups consecutive plain prose lines into one paragraph unit.
// Every byte of content lands in exactly one unit's text, so joining the texts gives the input back.
func split(content string) []unit {
	var units []unit
	inFence := false
	var para []string
	flush := func() {
		if len(para) == 0 {
			return
		}
		text := strings.Join(para, "")
		body := strings.TrimRight(text, "\r\n")
		lead := text[:len(body)-len(strings.TrimLeft(body, " \t"))]
		units = append(units, unit{text: text, prose: true, lead: lead, body: body[len(lead):], trail: text[len(body):], line: true})
		para = nil
	}
	for _, ln := range strings.SplitAfter(content, "\n") {
		if ln == "" {
			continue
		}
		trimmed := strings.TrimSpace(ln)
		switch {
		// Fenced code is announced as code and goes through verbatim, so it is not counted towards the
		// unfenced-code ratio.
		case fenceRe.MatchString(ln):
			flush()
			inFence = !inFence
			units = append(units, unit{text: ln})
		case inFence:
			units = append(units, unit{text: ln})
		case trimmed == "":
			flush()
			units = append(units, unit{text: ln})
		case strings.HasPrefix(ln, "    ") || strings.HasPrefix(ln, "\t"),
			strings.HasPrefix(trimmed, "|"), strings.HasPrefix(trimmed, "#"), strings.HasPrefix(trimmed, "<"):
			flush()
			units = append(units, unit{text: ln, codeLike: !strings.HasPrefix(trimmed, "#"), line: true})
		case codeLineRe.MatchString(trimmed):
			flush()
			units = append(units, unit{text: ln, codeLike: true, line: true})
		default:
			if m := listRe.FindStringSubmatch(strings.TrimRight(ln, "\r\n")); m != nil {
				flush()
				units = append(units, unit{text: ln, prose: true, lead: m[1], body: m[2], trail: ln[len(m[1])+len(m[2]):], line: true})
				continue
			}
			para = append(para, ln)
		}
	}
	flush()
	return units
}

// modelClassifier runs the real model: words → ids (word by word, as kompress was trained), chunks of
// at most maxSeq tokens, one forward pass each. A word is kept if ANY of its tokens is — the model
// card's reference decoder does the same.
type modelClassifier struct {
	m   *Model
	tok *Tokenizer
}

func (mc *modelClassifier) tokens(word string) int { return len(mc.tok.EncodeWord(word)) }

func (mc *modelClassifier) keep(ctx context.Context, words []string) ([]bool, error) {
	keep := make([]bool, len(words))
	ids := []int{clsID}
	owner := []int{-1}
	run := func() error {
		if len(ids) == 1 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		logits := mc.m.Logits(append(ids, sepID))
		for i, w := range owner {
			if w >= 0 && logits[i][1] > logits[i][0] {
				keep[w] = true
			}
		}
		ids, owner = ids[:1], owner[:1]
		return nil
	}
	for wi, w := range words {
		wids := mc.tok.EncodeWord(w)
		if len(wids) > maxSeq-2 { // one enormous "word": keep it, show the model nothing
			keep[wi] = true
			continue
		}
		if len(ids)+len(wids)+1 > maxSeq {
			if err := run(); err != nil {
				return nil, err
			}
		}
		for _, id := range wids {
			ids = append(ids, id)
			owner = append(owner, wi)
		}
	}
	if err := run(); err != nil {
		return nil, err
	}
	return keep, nil
}
