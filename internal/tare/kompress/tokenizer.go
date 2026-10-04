package kompress

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/text/unicode/norm"
)

// ModernBERT's special token ids, checked against tokenizer.json at load.
const (
	unkID = 50280
	clsID = 50281
	sepID = 50282
)

// Tokenizer is ModernBERT's byte-level BPE (tokenizer.json: NFC normaliser, ByteLevel pre-tokeniser
// with add_prefix_space=false, BPE model) applied the way kompress was trained — one whitespace-split
// word at a time (`is_split_into_words=True`), so no word carries a leading-space marker.
type Tokenizer struct {
	vocab   map[string]int
	ranks   map[[2]string]int
	added   []addedToken // matched inside a word before BPE, longest first
	byteMap [256]string

	mu    sync.Mutex
	cache map[string][]int
}

type addedToken struct {
	content string
	id      int
}

// The GPT-2 pre-tokeniser pattern ByteLevel uses. Its whitespace alternatives never fire here — a
// word from strings.Fields has none — so Go's RE2 (no lookahead) is enough.
var preTokenize = regexp.MustCompile(`'s|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+`)

// wordCacheMax bounds the per-word cache so untrusted text cannot grow it without limit.
const wordCacheMax = 50000

// LoadTokenizer reads a Hugging Face tokenizer.json.
func LoadTokenizer(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tj struct {
		AddedTokens []struct {
			ID      int    `json:"id"`
			Content string `json:"content"`
		} `json:"added_tokens"`
		Model struct {
			Type   string            `json:"type"`
			Vocab  map[string]int    `json:"vocab"`
			Merges []json.RawMessage `json:"merges"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &tj); err != nil {
		return nil, fmt.Errorf("kompress: %s: %w", path, err)
	}
	if tj.Model.Type != "BPE" {
		return nil, fmt.Errorf("kompress: %s: model type %q, want BPE", path, tj.Model.Type)
	}
	t := &Tokenizer{vocab: tj.Model.Vocab, ranks: make(map[[2]string]int, len(tj.Model.Merges)), cache: map[string][]int{}}
	for i, m := range tj.Model.Merges {
		var pair []string
		if err := json.Unmarshal(m, &pair); err != nil { // older files write "a b"
			var s string
			if err := json.Unmarshal(m, &s); err != nil {
				return nil, fmt.Errorf("kompress: %s: merge %d: %w", path, i, err)
			}
			pair = strings.SplitN(s, " ", 2)
		}
		if len(pair) != 2 {
			return nil, fmt.Errorf("kompress: %s: merge %d is not a pair", path, i)
		}
		t.ranks[[2]string{pair[0], pair[1]}] = i
	}
	want := map[string]int{"[UNK]": unkID, "[CLS]": clsID, "[SEP]": sepID}
	for _, a := range tj.AddedTokens {
		if id, ok := want[a.Content]; ok {
			if a.ID != id {
				return nil, fmt.Errorf("kompress: %s: %s is id %d, want %d", path, a.Content, a.ID, id)
			}
			delete(want, a.Content)
		}
		if strings.TrimSpace(a.Content) == a.Content && a.Content != "" { // whitespace runs never occur inside a word
			t.added = append(t.added, addedToken{a.Content, a.ID})
		}
	}
	if len(want) > 0 {
		return nil, fmt.Errorf("kompress: %s: special tokens missing: %v", path, want)
	}
	t.byteMap = bytesToUnicode()
	return t, nil
}

// bytesToUnicode is GPT-2's reversible byte → printable-rune table.
func bytesToUnicode() [256]string {
	var m [256]string
	n := 0
	for b := 0; b < 256; b++ {
		if (b >= '!' && b <= '~') || (b >= 0xA1 && b <= 0xAC) || (b >= 0xAE && b <= 0xFF) {
			m[b] = string(rune(b))
		} else {
			m[b] = string(rune(256 + n))
			n++
		}
	}
	return m
}

// EncodeWord returns the token ids of one whitespace-free word.
func (t *Tokenizer) EncodeWord(word string) []int {
	t.mu.Lock()
	ids, ok := t.cache[word]
	t.mu.Unlock()
	if ok {
		return ids
	}
	ids = t.encodeWord(word)
	t.mu.Lock()
	if len(t.cache) >= wordCacheMax {
		clear(t.cache)
	}
	t.cache[word] = ids
	t.mu.Unlock()
	return ids
}

func (t *Tokenizer) encodeWord(word string) []int {
	var ids []int
	for word != "" {
		at, tok := t.firstAdded(word)
		if at < 0 {
			return append(ids, t.encodePlain(word)...)
		}
		ids = append(ids, t.encodePlain(word[:at])...)
		ids = append(ids, tok.id)
		word = word[at+len(tok.content):]
	}
	return ids
}

// firstAdded finds the earliest added token in s, preferring the longest at a tie.
func (t *Tokenizer) firstAdded(s string) (int, addedToken) {
	best, bestTok := -1, addedToken{}
	for _, a := range t.added {
		if i := strings.Index(s, a.content); i >= 0 && (best < 0 || i < best || (i == best && len(a.content) > len(bestTok.content))) {
			best, bestTok = i, a
		}
	}
	return best, bestTok
}

func (t *Tokenizer) encodePlain(s string) []int {
	if s == "" {
		return nil
	}
	var ids []int
	for _, piece := range preTokenize.FindAllString(norm.NFC.String(s), -1) {
		var sym []string
		for i := 0; i < len(piece); i++ {
			sym = append(sym, t.byteMap[piece[i]])
		}
		for _, s := range t.bpe(sym) {
			if id, ok := t.vocab[s]; ok {
				ids = append(ids, id)
			} else {
				ids = append(ids, unkID)
			}
		}
	}
	return ids
}

// bpe merges adjacent symbols, lowest merge rank first, until no ranked pair remains.
func (t *Tokenizer) bpe(sym []string) []string {
	for len(sym) > 1 {
		best, at := -1, -1
		for i := 0; i+1 < len(sym); i++ {
			if r, ok := t.ranks[[2]string{sym[i], sym[i+1]}]; ok && (best < 0 || r < best) {
				best, at = r, i
			}
		}
		if at < 0 {
			break
		}
		a, b := sym[at], sym[at+1]
		out := sym[:0:0]
		for i := 0; i < len(sym); i++ {
			if i+1 < len(sym) && sym[i] == a && sym[i+1] == b {
				out = append(out, a+b)
				i++
			} else {
				out = append(out, sym[i])
			}
		}
		sym = out
	}
	return sym
}
