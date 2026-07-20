package proxy

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG 1 (deploy-behind-TLS): the proxy copied the client's `Accept-Encoding: gzip` onto the upstream
// request. Go's http.Transport only decompresses transparently when the CALLER did not set Accept-Encoding,
// so forwarding it left resp.Body gzip-compressed — which the proxy then served under
// `Content-Type: application/json` with NO `Content-Encoding`. A correct client trusts the header and gets
// binary garbage. It never reproduced locally because plain `curl` sends no Accept-Encoding (Go then adds
// its own and decompresses); a real SDK sends it and Caddy forwards it.
//
// The property under test: what the proxy hands the client must MATCH its headers — plain JSON body when
// we serve application/json. A 200 was returned throughout, so we assert on the actual bytes + header.

func gzipBody(t *testing.T, s string) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip") // like a provider honoring Accept-Encoding: gzip
		w.WriteHeader(http.StatusOK)
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, s)
		_ = gw.Close()
	}
}

func looksGzip(b []byte) bool { return len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b }

// Non-streaming JSON response (the reported bug): forward() must hand back a PLAIN JSON body even when the
// client asked for gzip, because the proxy serves it as application/json.
func TestForward_UpstreamGzip_ClientBodyIsPlainJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(gzipBody(t, `{"id":"msg_1","content":"hello world"}`)))
	defer upstream.Close()

	p := newProxyWithFallback(t, upstream.URL, "", "")
	cfg := p.configForProvider("openai")

	reqBody := `{"model":"gpt-4o","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(reqBody))
	r.Header.Set("Accept-Encoding", "gzip") // the deployed case: a real SDK / Claude Code sends this; Caddy forwards it

	resp, respBody, _, err := p.forward(context.Background(), r, []byte(reqBody), "gpt-4o", cfg)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer resp.Body.Close()

	if looksGzip(respBody) {
		t.Fatalf("forward returned a GZIP body (magic 1f 8b) — served as application/json it is binary garbage to the client")
	}
	var j map[string]any
	if err := json.Unmarshal(respBody, &j); err != nil {
		t.Fatalf("body the proxy would serve is not valid JSON (client gets garbage): %v", err)
	}
	if j["content"] != "hello world" {
		t.Errorf("decoded body wrong: %v", j)
	}
	// Transparent decompression makes Go strip Content-Encoding; a lingering gzip label over a plain body
	// is exactly the header/body mismatch we are closing.
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("resp Content-Encoding = %q while body is plain JSON — header/body mismatch", ce)
	}
}

// Streaming SSE response (the Claude-Code hot path): the client-facing stream must be the decoded SSE text,
// never gzip bytes.
func TestStream_UpstreamGzip_ClientGetsPlainSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, openAISSEBody)
		_ = gw.Close()
	}))
	defer srv.Close()

	p := newProxyWithFallback(t, srv.URL, "", "")

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip") // Claude Code / SDK sends this
	w := newFlushRecorder()

	p.HandleOpenAI(w, req)

	out := w.Body.Bytes()
	if looksGzip(out) {
		t.Fatalf("client received GZIP-framed SSE (magic 1f 8b) — a streaming client gets garbage")
	}
	if !strings.Contains(string(out), "hello ") || !strings.Contains(string(out), "world") {
		t.Fatalf("client SSE is missing the decoded deltas (stream was not decompressed): %q", out)
	}
	if ce := w.Header().Get("Content-Encoding"); ce == "gzip" {
		t.Errorf("client Content-Encoding=gzip over a plain SSE stream — header/body mismatch")
	}
}
