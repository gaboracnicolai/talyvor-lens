package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/inference"
)

// B17.11 — OPENAI'S PRO AND CODEX MODELS ANSWER THE CHAT. The buffered translation (chat body to Responses
// body, Responses reply to chat completion) is inference's (internal/inference/responses_api.go), shared with
// ProviderInferer.Infer; this file is the streamed leg.

// ─── The streamed leg: Responses events in, chat.completion.chunk events out ────────────────────────

type responsesStreamOps struct {
	url     string
	setAuth func(*http.Request)
	model   string
}

// responsesEvent is one `data:` line of a Responses stream (the `event:` lines repeat its type).
type responsesEvent struct {
	Type     string                    `json:"type"`
	Delta    string                    `json:"delta"`
	Response *inference.ResponsesReply `json:"response"`
}

func parseResponsesEvent(line []byte) (responsesEvent, bool) {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return responsesEvent{}, false
	}
	var ev responsesEvent
	if json.Unmarshal(bytes.TrimSpace(payload), &ev) != nil {
		return responsesEvent{}, false
	}
	return ev, true
}

// finished reports whether the event is the stream's last word on a finished answer.
func (ev responsesEvent) finished() bool {
	return ev.Type == "response.completed" || ev.Type == "response.incomplete"
}

func (o responsesStreamOps) upstreamURL() string         { return o.url }
func (o responsesStreamOps) applyAuth(req *http.Request) { o.setAuth(req) }
func (responsesStreamOps) applyPoolKey(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
}

// prepareBody translates the chat body. One that cannot be translated goes as it is, and OpenAI's own
// 400 reaches the client — the stream seam has no error return here.
func (responsesStreamOps) prepareBody(body []byte) []byte {
	if out, err := inference.ToResponsesBody(body); err == nil {
		return out
	}
	return body
}

func (responsesStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	ev, ok := parseResponsesEvent(line)
	if !ok {
		return false
	}
	switch ev.Type {
	case "response.output_text.delta":
		acc.WriteString(ev.Delta)
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	}
	return false
}

func (responsesStreamOps) endsAnswer(line []byte) bool {
	ev, ok := parseResponsesEvent(line)
	return ok && ev.finished()
}

func (responsesStreamOps) extractUsage(line []byte, u *streamUsage) {
	ev, ok := parseResponsesEvent(line)
	if !ok || !ev.finished() || ev.Response == nil || ev.Response.Usage == nil {
		return
	}
	ru := ev.Response.Usage
	cached := ru.Cached()
	u.inputTokens = ru.InputTokens
	u.outputTokens = ru.OutputTokens
	u.uncachedInputTokens = max(ru.InputTokens-cached, 0)
	u.cachedInputTokens = cached
	u.cacheWriteInputTokens = 0
	u.present = true
}

func (o responsesStreamOps) toClient(line []byte) [][]byte {
	ev, ok := parseResponsesEvent(line)
	if !ok {
		return nil // event: lines and blank separators — each chunk below carries its own
	}
	switch {
	case ev.Type == "response.output_text.delta" && ev.Delta != "":
		return openAIChunk(o.model, ev.Delta, "")
	case ev.finished():
		finish := "stop"
		if ev.Response != nil {
			finish = ev.Response.FinishReason()
		}
		return openAIChunk(o.model, "", finish)
	}
	return nil
}

func (o responsesStreamOps) clientTail(u streamUsage) [][]byte { return openAIUsageTail(o.model, u) }

func (responsesStreamOps) synthesizeCachePayload(accumulated string) []byte {
	return openAIStreamOps{}.synthesizeCachePayload(accumulated)
}

// responsesStreamFor is the streaming writer for a Responses-only OpenAI model.
func responsesStreamFor(chatURL string, setAuth func(*http.Request), model string) (streamOps, error) {
	u, ok := inference.ResponsesURLFor(chatURL)
	if !ok {
		return nil, fmt.Errorf("streaming %s needs OpenAI's /v1/responses, and %q has no such endpoint", model, chatURL)
	}
	return responsesStreamOps{url: u, setAuth: setAuth, model: model}, nil
}
