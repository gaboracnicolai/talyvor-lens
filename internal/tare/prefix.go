package tare

// prefix.go — Tare phase 1c: PREFIX STABILITY (W6.1.3).
//
// ⚠ WHY THIS IS THE ITEM THE SET CANNOT DO WITHOUT. A token saving that breaks the provider's
// prompt cache moves the bill NOWHERE: the tokens you stopped sending are re-charged as uncached
// input on the very next turn, and an economy that books that saving mints credit from nothing.
//
// ── WHAT "THE FROZEN PREFIX" IS IN THIS PRODUCT, MEASURED RATHER THAN ASSUMED ──────────────────
//
// It is THE SYSTEM PROMPT, and both providers say so in this repo's own code:
//
//   · ANTHROPIC — internal/templates/detector.go#ApplyAnthropicCaching puts
//     `cache_control: {type: ephemeral}` on the system block, and Anthropic caches everything up to
//     and INCLUDING the marked block. It is live: internal/proxy/proxy.go calls it, in the template-detection block of the serve path, whenever a
//     system prompt is extractable, the template is PINNED, and the provider is anthropic.
//   · OPENAI — ApplyOpenAICaching is deliberately a no-op, and its comment states the rule this
//     file enforces: "Provider-native caching kicks in as long as the system message stays FIRST
//     AND BYTE-IDENTICAL."
//
// So the rule is not a generic "compress the first N bytes". It is: ⚠ TARE MUST NEVER TOUCH THE
// SYSTEM PROMPT, AND MUST NOT DISTURB ANY MESSAGE BUT THE NEWEST. Everything a provider has already
// tokenised and cached goes upstream unchanged, byte for byte.
//
// ── IT SPLICES, FOR THE SAME REASON THE GO TRIMMER DOES ────────────────────────────────────────
//
// Unmarshalling the request and re-marshalling it would reorder the envelope's keys and restyle its
// whitespace. Even where a provider tokenises CONTENT rather than raw HTTP bytes — so that
// reordering is probably harmless — "probably harmless" is exactly the reasoning W6.1.3 forbids:
// ⚠ ASSERT THE BYTES, NOT THE INTENT. This locates the newest message's content span and replaces
// only that, so every byte before it is unchanged by construction rather than by argument.
//
// ⚠ NEVER DROPS HISTORY. No message is removed, merged or summarised. The only thing that changes
// is the CONTENT OF THE LAST MESSAGE, and only when the inner reducer returns something smaller.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Refusal reasons specific to the prefix-stable wrapper.
const (
	ReasonNoMessages      = "no messages array to reduce"
	ReasonLastNotString   = "the newest message's content is not a plain string"
	ReasonInnerRefused    = "the inner reducer returned the content unchanged"
	ReasonSpanNotLocated  = "could not locate the newest message's content span in the raw body"
	ReasonPrefixWouldMove = "the reduction would have changed a byte before the newest message"
)

// PrefixStable wraps a Reduction and applies it ONLY to the newest message's content.
type PrefixStable struct {
	inner   Reduction
	kind    Kind
	observe func(Refusal)
}

// NewPrefixStable wraps inner. `kind` is what the inner reducer is asked for — the content router
// that picks it per message is phase 1c's sibling, not this file.
func NewPrefixStable(inner Reduction, kind Kind) *PrefixStable {
	return &PrefixStable{inner: inner, kind: kind}
}

func (p *PrefixStable) WithObserver(f func(Refusal)) *PrefixStable { p.observe = f; return p }

func (p *PrefixStable) refuse(content []byte, kind Kind, reason string) ([]byte, int, int, error) {
	if p.observe != nil {
		p.observe(Refusal{Kind: kind, Bytes: len(content), Reason: reason})
	}
	t := EstimateTokens(content)
	return content, t, t, nil
}

// Reduce implements Reduction over a whole chat request body.
func (p *PrefixStable) Reduce(ctx context.Context, body []byte, kind Kind) ([]byte, int, int, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return p.refuse(body, kind, ReasonEmpty)
	}
	lo, hi, raw, err := lastMessageContentSpan(body)
	if err != nil {
		return p.refuse(body, kind, err.Error())
	}

	// The span is a JSON string literal; the inner reducer works on its DECODED value.
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return p.refuse(body, kind, ReasonLastNotString)
	}
	reduced, _, _, err := p.inner.Reduce(ctx, []byte(decoded), p.kind)
	if err != nil {
		return p.refuse(body, kind, ReasonInnerRefused)
	}
	if bytes.Equal(reduced, []byte(decoded)) {
		return p.refuse(body, kind, ReasonInnerRefused)
	}

	encoded, err := json.Marshal(string(reduced))
	if err != nil {
		return p.refuse(body, kind, ReasonReencodeFailed)
	}
	out := make([]byte, 0, len(body))
	out = append(out, body[:lo]...)
	out = append(out, encoded...)
	out = append(out, body[hi:]...)

	if len(out) >= len(body) {
		return p.refuse(body, kind, ReasonNotSmaller)
	}
	// ⚠ THE PREFIX IS RE-CHECKED HERE, NOT ONLY IN A TEST. Splicing makes it true by construction,
	// and this asserts the construction actually held for the body in front of us — the same
	// posture as the Go trimmer re-parsing its own output. A prefix that moved is a REFUSAL, never
	// a silently-shipped cache miss.
	if !bytes.Equal(out[:lo], body[:lo]) {
		return p.refuse(body, kind, ReasonPrefixWouldMove)
	}
	return out, EstimateTokens(body), EstimateTokens(out), nil
}

// lastMessageContentSpan finds the byte range of the LAST messages[].content string literal.
//
// ⚠ IT SCANS RATHER THAN UNMARSHALS, because the whole point is to know where the bytes ARE. It
// walks tokens with json.Decoder, tracking InputOffset, and records the span of every
// `"content": "<string>"` that appears inside the top-level `messages` array.
func lastMessageContentSpan(body []byte) (lo, hi int, raw []byte, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	depth := 0
	inMessages := false
	messagesDepth := -1
	pendingContent := false
	found := false

	for {
		before := dec.InputOffset()
		tok, terr := dec.Token()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return 0, 0, nil, errors.New(ReasonSpanNotLocated)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if inMessages && depth < messagesDepth {
					inMessages = false
				}
			}
			pendingContent = false
		case string:
			// A key at the top level naming the messages array.
			if depth == 1 && t == "messages" {
				inMessages = true
				messagesDepth = depth
				pendingContent = false
				continue
			}
			if pendingContent {
				// This token IS the content value. Its literal span runs from just before this
				// token to the decoder's current offset.
				lo, hi = int(before), int(dec.InputOffset())
				// Trim leading whitespace the decoder counted before the opening quote.
				for lo < hi && body[lo] != '"' {
					lo++
				}
				raw = body[lo:hi]
				found = true
				pendingContent = false
				continue
			}
			if inMessages && t == "content" {
				pendingContent = true
				continue
			}
			pendingContent = false
		default:
			pendingContent = false
		}
	}
	if !found {
		return 0, 0, nil, errors.New(ReasonNoMessages)
	}
	return lo, hi, raw, nil
}
