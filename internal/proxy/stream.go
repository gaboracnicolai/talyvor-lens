package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

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
	body []byte,
) error {
	return s.serve(w, r, provider, model, prompt, body, openAIStreamOps{
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
	body []byte,
) error {
	return s.serve(w, r, provider, model, prompt, body, anthropicStreamOps{
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
	// processLine inspects an SSE line, appends any extracted content to
	// the accumulator, and reports whether the stream should terminate.
	processLine(line []byte, accumulated *strings.Builder) (done bool)
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
	body []byte,
	ops streamOps,
) error {
	// Headers must be committed BEFORE the first write/flush.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, ops.upstreamURL(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "build upstream request: "+err.Error())
		return err
	}
	for name, values := range r.Header {
		if strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range values {
			upstreamReq.Header.Add(name, v)
		}
	}
	ops.applyAuth(upstreamReq)

	resp, err := s.proxy.httpClient.Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+err.Error())
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var accumulated strings.Builder
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

		if ops.processLine(line, &accumulated) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	// Cache the assembled response and notify the learner. Use a fresh
	// context so a client-cancelled request doesn't abort the cache write —
	// we already paid for the upstream call, no reason to drop the result.
	storeCtx := context.Background()
	cached := ops.synthesizeCachePayload(accumulated.String())
	s.proxy.storeCaches(storeCtx, provider, model, prompt, cached)
	s.proxy.recordTokenEvent(storeCtx, provider, model, prompt, cached, 0)
	return nil
}
