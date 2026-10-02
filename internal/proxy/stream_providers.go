package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/inference"
)

// B18.7 — EVERY PROVIDER STREAMS THROUGH ITS OWN UPSTREAM AND KEY.
//
// The streaming seam had two writers: OpenAI's, and Anthropic's for everything else — so a streamed
// request for a Google, Mistral, Groq, vLLM or Bedrock model was sent to Anthropic's URL with
// Anthropic's key. providerStreamOps picks the writer from the request's own provider config, its URL
// and its auth; a provider with no writer is refused, never sent somewhere else.
//
// What the CLIENT receives: Anthropic's events for Anthropic; OpenAI chat.completion.chunk events for
// every other provider — the OpenAI-compatible ones natively, Google and Bedrock translated, since
// their handlers take and return the OpenAI shape.

// streamLineSource is an optional streamOps capability: the upstream's lines, for a provider whose
// stream is not text lines (Bedrock's binary event stream). The default reads SSE lines.
type streamLineSource interface {
	lines(body io.Reader) lineSource
}

type lineSource interface {
	Next() bool
	Line() []byte
	Err() error
}

// clientTranslator is an optional streamOps capability: what the client receives for one upstream
// line, and after the last one — given the usage the stream reported, which is what it is billed on.
// The default forwards each line as it came.
type clientTranslator interface {
	toClient(line []byte) [][]byte
	clientTail(u streamUsage) [][]byte
}

type scannerSource struct{ s *bufio.Scanner }

func (s scannerSource) Next() bool   { return s.s.Scan() }
func (s scannerSource) Line() []byte { return s.s.Bytes() }
func (s scannerSource) Err() error   { return s.s.Err() }

func upstreamLines(ops streamOps, body io.Reader) lineSource {
	if src, ok := ops.(streamLineSource); ok {
		return src.lines(body)
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), sseScannerMax)
	return scannerSource{sc}
}

// providerStreamOps is the writer for cfg's provider, streaming model. An error means the provider
// has no streaming writer and the request must be refused.
func providerStreamOps(cfg providerConfig, model string) (streamOps, error) {
	switch name := cfg.ProviderName(); name {
	case "openai", "mistral", "groq", "vllm":
		if name == "openai" && inference.ResponsesOnly(model) {
			return responsesStreamFor(cfg.UpstreamURL(model), cfg.ApplyAuth, model) // B17.11
		}
		return openAIStreamOps{url: cfg.UpstreamURL(model), setAuth: cfg.ApplyAuth}, nil
	case "anthropic":
		return anthropicStreamOps{url: cfg.UpstreamURL(model), setAuth: cfg.ApplyAuth}, nil
	case "google":
		return geminiStreamOps{url: geminiStreamURL(cfg.UpstreamURL(model)), model: model}, nil
	case "bedrock":
		return bedrockStreamOps{url: strings.Replace(cfg.UpstreamURL(model), "/invoke", "/invoke-with-response-stream", 1),
			setAuth: cfg.ApplyAuth, model: model}, nil
	default:
		return nil, fmt.Errorf("streaming is not supported for provider %q", name)
	}
}

// openAIChunk is one chat.completion.chunk event line, then the blank line that ends it.
func openAIChunk(model, content, finish string) [][]byte {
	choice := map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}
	if finish != "" {
		choice["delta"] = map[string]any{}
		choice["finish_reason"] = finish
	}
	b, _ := json.Marshal(map[string]any{"object": "chat.completion.chunk", "model": model, "choices": []any{choice}})
	return [][]byte{append([]byte("data: "), b...), {}}
}

var openAIDoneLines = [][]byte{[]byte("data: " + openAIDoneMarker), {}}

// openAIUsageTail ends a translated stream the way an OpenAI stream with include_usage ends: a
// usage-only chunk, then [DONE] (B18.59). The counts are the ones Lens read from the provider and
// bills; prompt_tokens is the whole input, cache reads and writes included, as OpenAI counts it.
// A stream that reported no usage ends with [DONE] alone rather than with invented zeros.
func openAIUsageTail(model string, u streamUsage) [][]byte {
	if !u.present {
		return openAIDoneLines
	}
	prompt := u.uncachedInputTokens + u.cachedInputTokens + u.cacheWriteInputTokens
	if prompt == 0 {
		prompt = u.inputTokens
	}
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": u.outputTokens, "total_tokens": prompt + u.outputTokens}
	if u.cachedInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": u.cachedInputTokens}
	}
	b, _ := json.Marshal(map[string]any{"object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": usage})
	return append([][]byte{append([]byte("data: "), b...), {}}, openAIDoneLines...)
}

// ─── Google: Gemini's streamGenerateContent, SSE ─────────────────────────────

type geminiStreamOps struct {
	url   string
	model string
}

// geminiStreamURL turns generateContent?key=… into streamGenerateContent?alt=sse&key=….
func geminiStreamURL(u string) string {
	return strings.Replace(u, ":generateContent?", ":streamGenerateContent?alt=sse&", 1)
}

func (g geminiStreamOps) upstreamURL() string   { return g.url }
func (geminiStreamOps) applyAuth(*http.Request) {} // the key is in the URL, as on the buffered path
func (geminiStreamOps) applyPoolKey(req *http.Request, key string) {
	q := req.URL.Query()
	q.Set("key", key)
	req.URL.RawQuery = q.Encode()
}

func (geminiStreamOps) prepareBody(body []byte) []byte {
	if out, _, err := inference.TranslateToGemini(body); err == nil {
		return out
	}
	return body
}

type geminiChunk struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

func parseGeminiLine(line []byte) (geminiChunk, bool) {
	var c geminiChunk
	if !bytes.HasPrefix(line, []byte("data:")) {
		return c, false
	}
	return c, json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &c) == nil
}

func (c geminiChunk) text() string {
	var b strings.Builder
	for _, cand := range c.Candidates {
		for _, p := range cand.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func (c geminiChunk) finished() bool {
	for _, cand := range c.Candidates {
		if cand.FinishReason != "" {
			return true
		}
	}
	return false
}

func (geminiStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	if c, ok := parseGeminiLine(line); ok {
		acc.WriteString(c.text())
	}
	return false // Gemini's stream ends when the connection closes
}

func (geminiStreamOps) endsAnswer(line []byte) bool {
	c, ok := parseGeminiLine(line)
	return ok && c.finished()
}

func (geminiStreamOps) extractUsage(line []byte, u *streamUsage) {
	c, ok := parseGeminiLine(line)
	if !ok || c.UsageMetadata == nil || c.UsageMetadata.PromptTokenCount+c.UsageMetadata.CandidatesTokenCount == 0 {
		return
	}
	m := c.UsageMetadata
	u.inputTokens = m.PromptTokenCount
	u.outputTokens = m.CandidatesTokenCount
	u.cachedInputTokens = m.CachedContentTokenCount
	u.uncachedInputTokens = max(m.PromptTokenCount-m.CachedContentTokenCount, 0)
	u.present = true
}

func (g geminiStreamOps) toClient(line []byte) [][]byte {
	c, ok := parseGeminiLine(line)
	if !ok {
		return nil // Gemini's blank separators: the chunks below carry their own
	}
	var out [][]byte
	if t := c.text(); t != "" {
		out = append(out, openAIChunk(g.model, t, "")...)
	}
	if c.finished() {
		out = append(out, openAIChunk(g.model, "", "stop")...)
	}
	return out
}

func (g geminiStreamOps) clientTail(u streamUsage) [][]byte { return openAIUsageTail(g.model, u) }

func (geminiStreamOps) synthesizeCachePayload(accumulated string) []byte {
	return openAIStreamOps{}.synthesizeCachePayload(accumulated)
}

// ─── Bedrock: Anthropic on Bedrock, invoke-with-response-stream ─────────────
//
// The response is AWS's binary event stream. Each "chunk" event carries {"bytes": base64(one
// Anthropic streaming event)}; decoded, it is exactly the event Anthropic's own stream carries, so
// the Anthropic reading of usage, text and end is reused on it.

type bedrockStreamOps struct {
	url     string
	setAuth func(*http.Request)
	model   string
}

func (b bedrockStreamOps) upstreamURL() string { return b.url }

// applyAuth sets Bedrock's own content headers — the client's (e.g. Accept: text/event-stream) were
// copied onto the request — then signs, SigV4 over the final headers and body.
func (b bedrockStreamOps) applyAuth(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	b.setAuth(req)
}
func (bedrockStreamOps) applyPoolKey(*http.Request, string) {}

func (bedrockStreamOps) prepareBody(body []byte) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		delete(m, "stream") // the endpoint streams; the field is not part of Bedrock's Anthropic body
		delete(m, "stream_options")
		if b, err := json.Marshal(m); err == nil {
			body = b
		}
	}
	if out, err := inference.TranslateToBedrockFormat(body); err == nil {
		return out
	}
	return body
}

func (bedrockStreamOps) lines(body io.Reader) lineSource { return &eventStreamSource{r: body} }

func (bedrockStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	return anthropicStreamOps{}.processLine(line, acc)
}
func (bedrockStreamOps) endsAnswer(line []byte) bool { return anthropicStreamOps{}.endsAnswer(line) }
func (bedrockStreamOps) extractUsage(line []byte, u *streamUsage) {
	anthropicStreamOps{}.extractUsage(line, u)
}

func (b bedrockStreamOps) toClient(line []byte) [][]byte {
	var ev struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if !bytes.HasPrefix(line, []byte("data:")) || json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &ev) != nil {
		return nil
	}
	switch {
	case ev.Type == "content_block_delta" && ev.Delta.Text != "":
		return openAIChunk(b.model, ev.Delta.Text, "")
	case ev.Type == "message_stop":
		return openAIChunk(b.model, "", "stop")
	}
	return nil
}

func (b bedrockStreamOps) clientTail(u streamUsage) [][]byte { return openAIUsageTail(b.model, u) }

func (bedrockStreamOps) synthesizeCachePayload(accumulated string) []byte {
	return openAIStreamOps{}.synthesizeCachePayload(accumulated)
}

// eventStreamSource decodes AWS event-stream messages — a 12-byte prelude (total length, headers
// length, prelude CRC), the headers, the payload, a 4-byte message CRC — into "data: <event>" lines.
// An exception message ends the stream with an error: the answer is then never cached.
type eventStreamSource struct {
	r    io.Reader
	line []byte
	err  error
}

func (s *eventStreamSource) Next() bool {
	for s.err == nil {
		var prelude [12]byte
		if _, err := io.ReadFull(s.r, prelude[:]); err != nil {
			if err != io.EOF {
				s.err = err
			}
			return false
		}
		total := binary.BigEndian.Uint32(prelude[0:4])
		hlen := binary.BigEndian.Uint32(prelude[4:8])
		if total < 16+hlen || total > 16<<20 {
			s.err = fmt.Errorf("bedrock event stream: bad message length %d", total)
			return false
		}
		rest := make([]byte, total-12)
		if _, err := io.ReadFull(s.r, rest); err != nil {
			s.err = err
			return false
		}
		headers := eventStreamHeaders(rest[:hlen])
		payload := rest[hlen : len(rest)-4]
		if headers[":message-type"] == "exception" || headers[":message-type"] == "error" {
			s.err = fmt.Errorf("bedrock stream %s: %s", headers[":exception-type"], payload)
			return false
		}
		if headers[":event-type"] != "chunk" {
			continue
		}
		var chunk struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(payload, &chunk) != nil {
			continue
		}
		ev, err := base64.StdEncoding.DecodeString(chunk.Bytes)
		if err != nil {
			continue
		}
		s.line = append([]byte("data: "), ev...)
		return true
	}
	return false
}

func (s *eventStreamSource) Line() []byte { return s.line }
func (s *eventStreamSource) Err() error   { return s.err }

// eventStreamHeaders reads the string-valued headers (type 7) a Bedrock message carries.
func eventStreamHeaders(b []byte) map[string]string {
	h := map[string]string{}
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 1+n+1 {
			break
		}
		name := string(b[1 : 1+n])
		typ := b[1+n]
		b = b[2+n:]
		if typ != 7 || len(b) < 2 {
			break // Bedrock sends only string headers; anything else ends the read safely
		}
		vl := int(binary.BigEndian.Uint16(b[:2]))
		if len(b) < 2+vl {
			break
		}
		h[name] = string(b[2 : 2+vl])
		b = b[2+vl:]
	}
	return h
}
