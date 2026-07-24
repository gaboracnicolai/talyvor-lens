package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/workspace"
)

// streamUsage accumulates the provider's reported token counts as they
// arrive across the stream (OpenAI's final usage chunk; Anthropic's
// message_start input + message_delta output). present=false means the
// stream surfaced no usage, so the caller bills via the len/4 estimate.
type streamUsage struct {
	inputTokens  int
	outputTokens int
	// Cache-aware breakdown (mirrors inference.Usage), populated by extractUsage so the streamed settle
	// prices cache reads/writes at their own rate like the buffered path. Zero when the provider reports
	// no caching → uncachedInputTokens == inputTokens and the basis is flat, identical to before.
	uncachedInputTokens   int
	cachedInputTokens     int
	cacheWriteInputTokens int
	present               bool
}

// streamSpend carries the per-request billing context into the stream
// handler so a completed stream can record spend — the piece that used to be
// missing, leaving streamed requests invisible to budgets/alerts.
type streamSpend struct {
	wsID, team, sprint, feature string
	model                       string
	requestID, sessionID        string
	modality                    string
	logging                     workspace.LoggingPolicy
	estInputTokens              int // fallback input estimate when no usage is emitted
}

const (
	openAIDoneMarker = "[DONE]"
	sseScannerMax    = 1 << 20 // 1 MiB per SSE line, plenty for any chunk
)

// StreamHandler proxies streaming LLM responses back to the client SSE-style
// while accumulating the assembled text content for caching + learning.
type StreamHandler struct {
	proxy *Proxy
}

// ServeOpenAI streams an OpenAI chat-completions response. The accumulated
// text content is cached as a non-streaming JSON shape so future cache hits
// (streaming or not) can return a complete response immediately.
func (s *StreamHandler) ServeOpenAI(
	w http.ResponseWriter,
	r *http.Request,
	provider string,
	model string,
	prompt string,
	cachePrompt string,
	body []byte,
	piiDetected bool,
	sc streamSpend,
) error {
	return s.serve(w, r, provider, model, prompt, cachePrompt, body, piiDetected, sc, openAIStreamOps{
		url:     s.proxy.openAIURL,
		setAuth: func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+s.proxy.openAIKey) },
	})
}

// ServeAnthropic streams an Anthropic /v1/messages response.
func (s *StreamHandler) ServeAnthropic(
	w http.ResponseWriter,
	r *http.Request,
	provider string,
	model string,
	prompt string,
	cachePrompt string,
	body []byte,
	piiDetected bool,
	sc streamSpend,
) error {
	return s.serve(w, r, provider, model, prompt, cachePrompt, body, piiDetected, sc, anthropicStreamOps{
		url: s.proxy.anthropicURL,
		setAuth: func(req *http.Request) {
			req.Header.Set("x-api-key", s.proxy.anthropicKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		},
	})
}

// streamOps abstracts the per-provider differences in stream parsing and
// cache-payload synthesis. The transport mechanics (fetch upstream, scan
// SSE lines, flush, accumulate, store) are identical and live in serve().
type streamOps interface {
	upstreamURL() string
	applyAuth(*http.Request)
	// prepareBody adjusts the upstream request body before streaming — e.g.
	// injecting stream_options.include_usage for OpenAI-family providers so
	// the final chunk carries a usage block. Identity for providers that
	// emit usage natively (Anthropic).
	prepareBody(body []byte) []byte
	// processLine inspects an SSE line, appends any extracted content to
	// the accumulator, and reports whether the stream should terminate.
	processLine(line []byte, accumulated *strings.Builder) (done bool)
	// extractUsage inspects one SSE line for provider-reported usage,
	// updating u in place. No-op for lines that carry no usage.
	extractUsage(line []byte, u *streamUsage)
	// synthesizeCachePayload produces the JSON to cache once the stream
	// has fully been consumed.
	synthesizeCachePayload(accumulated string) []byte
}

type openAIStreamOps struct {
	url     string
	setAuth func(*http.Request)
}

func (o openAIStreamOps) upstreamURL() string         { return o.url }
func (o openAIStreamOps) applyAuth(req *http.Request) { o.setAuth(req) }

// prepareBody injects stream_options.include_usage so OpenAI-compatible
// providers emit a final usage chunk. Best-effort: a parse failure returns
// the body untouched — metering must never break the stream.
func (openAIStreamOps) prepareBody(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	m["stream_options"] = so
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// extractUsage reads the final chunk's usage block (present once
// include_usage is set). Intermediate chunks carry usage:null and are
// skipped via the pointer.
func (openAIStreamOps) extractUsage(line []byte, u *streamUsage) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte(openAIDoneMarker)) {
		return
	}
	var chunk struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil || chunk.Usage == nil {
		return
	}
	cached, write := 0, 0
	if d := chunk.Usage.PromptTokensDetails; d != nil {
		cached, write = d.CachedTokens, d.CacheWriteTokens
	}
	uncached := chunk.Usage.PromptTokens - cached - write // cached/write are a subset of prompt_tokens
	if uncached < 0 {
		uncached = 0
	}
	u.inputTokens = chunk.Usage.PromptTokens // legacy total (incl cached) — unchanged
	u.outputTokens = chunk.Usage.CompletionTokens
	u.uncachedInputTokens = uncached
	u.cachedInputTokens = cached
	u.cacheWriteInputTokens = write
	u.present = true
}

func (openAIStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte(openAIDoneMarker)) {
		return true
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	for _, c := range chunk.Choices {
		acc.WriteString(c.Delta.Content)
	}
	return false
}

func (openAIStreamOps) synthesizeCachePayload(accumulated string) []byte {
	out, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": accumulated}},
		},
	})
	return out
}

type anthropicStreamOps struct {
	url     string
	setAuth func(*http.Request)
}

func (a anthropicStreamOps) upstreamURL() string         { return a.url }
func (a anthropicStreamOps) applyAuth(req *http.Request) { a.setAuth(req) }

// prepareBody is identity: Anthropic emits usage natively (message_start +
// message_delta), so there is no include_usage flag to inject.
func (anthropicStreamOps) prepareBody(body []byte) []byte { return body }

// extractUsage reuses the canonical Anthropic chunk normaliser
// (internal/cache) for OUTPUT tokens rather than re-parsing message_delta
// usage here, and parses the message_start event — which that normaliser
// (content + output focused) doesn't surface — for INPUT tokens.
func (anthropicStreamOps) extractUsage(line []byte, u *streamUsage) {
	if chunk, ok := cache.NormalizeAnthropicChunk(string(line)); ok && chunk.IsFinal && chunk.TokenDelta > 0 {
		u.outputTokens = chunk.TokenDelta
		u.present = true
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &ev) == nil && ev.Type == "message_start" && ev.Message.Usage.InputTokens > 0 {
		un := ev.Message.Usage
		u.inputTokens = un.InputTokens // legacy: uncached only (cache read/write are disjoint) — unchanged
		u.uncachedInputTokens = un.InputTokens
		u.cachedInputTokens = un.CacheReadInputTokens
		u.cacheWriteInputTokens = un.CacheCreationInputTokens
		u.present = true
	}
}

func (anthropicStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var chunk struct {
		Type  string `json:"type"`
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	if chunk.Type == "content_block_delta" {
		acc.WriteString(chunk.Delta.Text)
	}
	// Anthropic doesn't have a single "DONE" marker like OpenAI; the
	// connection close ends the stream. We could return true on
	// message_stop to break early, but EOF works fine too — keep it simple.
	return false
}

func (anthropicStreamOps) synthesizeCachePayload(accumulated string) []byte {
	out, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": accumulated},
		},
	})
	return out
}

func (s *StreamHandler) serve(
	w http.ResponseWriter,
	r *http.Request,
	provider string,
	model string,
	prompt string,
	cachePrompt string,
	body []byte,
	piiDetected bool,
	sc streamSpend,
	ops streamOps,
) error {
	// Headers must be committed BEFORE the first write/flush.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Ask the provider to surface usage in the stream (OpenAI-family:
	// stream_options.include_usage; identity for Anthropic). Best-effort.
	body = ops.prepareBody(body)

	// Retry the initial upstream call on transient failures. Once we
	// commit to streaming (after WriteHeader below) there's no second
	// chance, so retries only cover the connection-establishment phase.
	// Measure the upstream connection/initial-response latency (the
	// retry.Do block), NOT the whole stream forward. Record on every outcome
	// with a bounded provider+status label; never alters control flow/errors.
	upstreamStart := time.Now()
	result := retry.Do(r.Context(), retry.DefaultConfig(), func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, ops.upstreamURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for name, values := range r.Header {
			// Skip Host, and Accept-Encoding: forwarding the client's Accept-Encoding disables Go's
			// transparent gzip decoding, so a gzipped upstream stream would be relayed as compressed
			// bytes under text/event-stream — garbage to a streaming client. Let the transport decode.
			if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Accept-Encoding") {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		ops.applyAuth(req)
		return s.proxy.httpClient.Do(req)
	})
	if result.LastError != nil {
		metrics.RecordUpstream(upstreamProviderLabel(provider), "error", time.Since(upstreamStart))
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+result.LastError.Error())
		return result.LastError
	}
	resp := result.Response
	defer resp.Body.Close()
	metrics.RecordUpstream(upstreamProviderLabel(provider), upstreamStatusClass(resp, nil), time.Since(upstreamStart))

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	if result.Attempts > 1 {
		w.Header().Set("X-Talyvor-Attempts", strconv.Itoa(result.Attempts))
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var accumulated strings.Builder
	var usage streamUsage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseScannerMax)

	for scanner.Scan() {
		// Stop forwarding if the client disconnected.
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}

		line := scanner.Bytes()
		// Reconstruct the SSE wire format on the way out: each scanner line
		// strips its trailing \n; the original blank-line separators arrive
		// as zero-length lines. Writing line+"\n" round-trips both.
		_, _ = w.Write(line)
		_, _ = w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}

		ops.extractUsage(line, &usage)
		if ops.processLine(line, &accumulated) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	// Post-serve seam (cache write, learner, streamed spend + reservation SETTLE). Detach from the
	// request's cancellation so a client that disconnects after the stream completes can't abort the
	// bill — we already paid the provider. WithoutCancel (NOT Background) keeps r's VALUES, including
	// the reservation handle threaded onto r in serve(), so the streamed settle actually FIRES in-band
	// instead of stranding the hold for the sweeper to refund (which made streaming requests free).
	storeCtx := context.WithoutCancel(r.Context())
	cached := ops.synthesizeCachePayload(accumulated.String())
	shouldCache := !piiDetected
	if shouldCache && s.proxy.scorer != nil {
		// Score against the accumulated text content, not the synthesized
		// JSON, so the heuristics see what the user would see.
		q := s.proxy.scorer.ScoreResponse(storeCtx, prompt, accumulated.String(), provider, model)
		if !q.ShouldCache {
			shouldCache = false
		}
	}
	if shouldCache {
		// Use the workspace-scoped prompt for the cache key so streamed
		// responses respect tenant isolation just like buffered ones. The raw
		// prompt + wsID also feed the opt-in pooled (cross-tenant) write.
		s.proxy.storeCaches(storeCtx, provider, model, cachePrompt, prompt, sc.wsID, cached)
	}
	eventPrompt := prompt
	if piiDetected && s.proxy.piiDetector != nil {
		eventPrompt = s.proxy.piiDetector.Detect(prompt).Redacted
	}
	// Gated on the logging policy inside recordTokenEvent (full only) — sc.wsID
	// is the same workspace recordStreamSpend below reads its policy from, so a
	// none/metadata streamed request feeds neither the learner nor the spend row.
	s.proxy.recordTokenEvent(storeCtx, provider, model, eventPrompt, cached, 0, piiDetected, sc.wsID)
	// Close the streamed-spend gap: bill on the captured provider usage when
	// present, else the len/4 estimate. A streamed request must never again
	// be invisible to budgets/alerts.
	s.proxy.recordStreamSpend(storeCtx, sc, usage, accumulated.String())
	return nil
}
