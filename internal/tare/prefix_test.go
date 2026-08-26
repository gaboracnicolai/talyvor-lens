package tare_test

// prefix_test.go — W6.1.3: prefix stability.
//
// ⚠ THE ITEM'S DEMAND IS LITERAL AND SO ARE THESE TESTS: "ASSERT THE BYTES, NOT THE INTENT: a test
// that sends turn N and turn N+1 and compares the prefix byte-for-byte." Not "the system prompt is
// still present", not "the messages are equal after decoding" — the BYTES.
//
// ⚠ WHY IT MATTERS, IN THIS PRODUCT'S OWN TERMS. internal/templates/detector.go puts Anthropic's
// `cache_control: ephemeral` on the SYSTEM block (live, from internal/proxy/proxy.go's template-detection block, once a template is
// pinned), and Anthropic caches everything up to and including it. ApplyOpenAICaching is a no-op
// whose comment states the same rule for OpenAI: caching "kicks in as long as the system message
// stays FIRST AND BYTE-IDENTICAL". So a reducer that rewrites the system prompt — or merely
// re-serialises the envelope around it — is spending the cache to save the tokens.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/tare"
)

// conversation builds a request whose system prompt is long and stable and whose LAST message
// carries the new bytes — the shape a chat turn actually has.
func conversation(t *testing.T, turns int, newest string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"model":"claude-haiku-4-5","system":[{"type":"text","text":"You are a careful assistant. Keep the system prompt long and stable so the provider caches it.","cache_control":{"type":"ephemeral"}}],"messages":[`)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"role":"user","content":"turn %d question"},{"role":"assistant","content":"turn %d answer"}`, i, i)
	}
	if turns > 0 {
		b.WriteString(",")
	}
	fmt.Fprintf(&b, `{"role":"user","content":%s}`, mustJSONString(t, newest))
	b.WriteString(`]}`)
	return []byte(b.String())
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// toolOutput is the kind of payload Tare exists to shrink: an array of same-shaped rows.
func toolOutput(n int) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, fmt.Sprintf(`{"path":"pkg/mod/file_%d.go","line":%d,"rule":"unused","severity":"warning"}`, i, i*3))
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func prefixReduce(t *testing.T, body []byte) []byte {
	t.Helper()
	r := tare.NewPrefixStable(tare.NewJSONReducer(), tare.KindJSON)
	out, _, _, err := r.Reduce(context.Background(), body, tare.KindUnknown)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	return out
}

// ⚠ THE HEADLINE, AND IT IS THE ITEM'S TEST VERBATIM: turn N, turn N+1, compare the prefix
// BYTE-FOR-BYTE.
func TestPrefix_TurnNAndTurnNPlusOneShareAByteIdenticalPrefix(t *testing.T) {
	turnN := conversation(t, 3, toolOutput(30))
	turnN1 := conversation(t, 4, toolOutput(30))

	outN := prefixReduce(t, turnN)
	outN1 := prefixReduce(t, turnN1)

	if len(outN) >= len(turnN) || len(outN1) >= len(turnN1) {
		t.Fatalf("nothing was reduced (%d->%d, %d->%d) — then 'the prefix is stable' is trivially "+
			"true and this test asserts nothing", len(turnN), len(outN), len(turnN1), len(outN1))
	}

	// The frozen prefix is everything the shorter (earlier) request and the longer one have in
	// common at the front — in particular the whole system block.
	shared := commonPrefixLen(outN, outN1)
	sysEnd := bytes.Index(turnN, []byte(`"messages"`))
	if sysEnd < 0 {
		t.Fatal("fixture has no messages key")
	}
	if shared < sysEnd {
		t.Fatalf("THE PREFIX MOVED. Turn N and turn N+1 agree for only %d bytes, but the system "+
			"block alone runs to byte %d.\n⚠ Anthropic's cache_control sits on that system block "+
			"and OpenAI caches it only while it stays first and byte-identical, so every byte of "+
			"divergence before it is a cache miss the token saving has to pay for.\n N: %s\nN+1: %s",
			shared, sysEnd, head(outN, shared+40), head(outN1, shared+40))
	}

	// And the system block itself must be present unchanged in both.
	sysBlock := turnN[:sysEnd]
	for i, out := range [][]byte{outN, outN1} {
		if !bytes.HasPrefix(out, sysBlock) {
			t.Fatalf("output %d does not begin with the original system block byte-for-byte:\n%s", i, head(out, 200))
		}
	}
}

// ⚠ EVERY EARLIER MESSAGE IS UNTOUCHED, not just the system prompt. History is never dropped,
// merged or summarised — the item says so and this asserts it on the bytes.
func TestPrefix_OnlyTheNewestMessageChanges(t *testing.T) {
	body := conversation(t, 3, toolOutput(30))
	out := prefixReduce(t, body)

	// ⚠ CONTROL J2 EXPOSED THIS TEST AS VACUOUS WITHOUT THIS LINE. Pointing the span finder at the
	// FIRST message instead of the last makes the inner reducer refuse (turn 0's content is prose),
	// so the whole thing refuses, the body comes back unchanged — and "nothing before the newest
	// message changed" is then trivially true. A prefix assertion has to be made over an actual
	// reduction or it is asserting the stability of a no-op.
	if len(out) >= len(body) {
		t.Fatalf("nothing was reduced (%d -> %d), so every assertion below is satisfied by the "+
			"input being handed straight back", len(body), len(out))
	}

	lastStart := bytes.LastIndex(body, []byte(`{"role":"user","content":`))
	if lastStart < 0 {
		t.Fatal("fixture shape changed")
	}
	if !bytes.Equal(out[:lastStart], body[:lastStart]) {
		t.Fatalf("a byte changed BEFORE the newest message.\nwant prefix: %s\ngot:         %s",
			head(body[:lastStart], 160), head(out[:lastStart], 160))
	}
	// Every earlier turn's text still there, verbatim.
	for i := 0; i < 3; i++ {
		for _, needle := range []string{
			fmt.Sprintf(`"turn %d question"`, i),
			fmt.Sprintf(`"turn %d answer"`, i),
		} {
			if !bytes.Contains(out, []byte(needle)) {
				t.Fatalf("history was dropped: %s is gone", needle)
			}
		}
	}
}

// ⚠ THE ENVELOPE IS NOT RE-SERIALISED. Unmarshal/marshal would reorder keys and restyle whitespace;
// the item's rule is about bytes, so the transform must splice.
func TestPrefix_TheEnvelopeIsSplicedNotReserialised(t *testing.T) {
	// Deliberately odd key order and spacing: a re-serialisation would normalise both.
	body := []byte(`{"messages":[{"content":` + string(mustJSONStringRaw(toolOutput(30))) + `,"role":"user"}]  ,  "model":"m","system":"s"}`)
	out := prefixReduce(t, body)
	if !bytes.HasSuffix(out, []byte(`,"role":"user"}]  ,  "model":"m","system":"s"}`)) {
		t.Fatalf("the envelope was re-serialised — key order or whitespace changed after the "+
			"content.\n%s", head(out, 300))
	}
}

func mustJSONStringRaw(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// ⚠ THE REDUCED CONTENT IS STILL THE SAME DOCUMENT. Prefix stability must not come at the cost of
// the phase-1a guarantee.
func TestPrefix_TheNewestMessageStillRoundTrips(t *testing.T) {
	original := toolOutput(30)
	body := conversation(t, 2, original)
	out := prefixReduce(t, body)

	var got struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the spliced body is not valid JSON: %v\n%s", err, head(out, 300))
	}
	last := got.Messages[len(got.Messages)-1].Content
	mustRoundTrip(t, []byte(original), []byte(last))
}

// Refusals: unchanged body, nil error, a reason.
func TestPrefix_RefusesAndReturnsTheInputUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, in, wantReason string }{
		{"empty", ``, tare.ReasonEmpty},
		{"no messages", `{"model":"m","system":"s"}`, tare.ReasonNoMessages},
		{"content not reducible", `{"messages":[{"role":"user","content":"just prose"}]}`, tare.ReasonInnerRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got tare.Refusal
			r := tare.NewPrefixStable(tare.NewJSONReducer(), tare.KindJSON).
				WithObserver(func(f tare.Refusal) { got = f })
			out, tin, tout, err := r.Reduce(context.Background(), []byte(tc.in), tare.KindUnknown)
			if err != nil {
				t.Fatalf("a refusal must not be an error: %v", err)
			}
			if !bytes.Equal(out, []byte(tc.in)) {
				t.Fatalf("a refusal must return the input UNCHANGED:\n in: %q\nout: %q", tc.in, out)
			}
			if tin != tout {
				t.Fatalf("a refusal changed the token estimate: %d -> %d", tin, tout)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func head(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
