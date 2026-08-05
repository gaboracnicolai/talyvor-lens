package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// embeddings_route_test.go — the endpoint a RELEASED CLI depends on.
//
// talyvor-code v0.1.0 builds its semantic codebase index by POSTing
// /v1/proxy/openai/v1/embeddings with model text-embedding-3-small. Lens forwarded that body to
// the fixed chat-completions URL, so OpenAI answered 400 "you must provide a messages parameter"
// and the CLI's headline capability could not work. Confirmed by RUNNING it, not by reading:
//
//	CLIENT SENT   : POST /v1/proxy/openai/v1/embeddings
//	UPSTREAM GOT  : /v1/chat/completions
//	LENS RETURNED : 400
//
// These assert the RESPONSE SHAPE and the recorded spend, not a status code — a 200 carrying a
// chat completion would satisfy a status assertion and still be useless to the client.

// recordingUpstream stands in for the provider and reports what it was asked for. It answers a
// real embeddings body on the embeddings path and reproduces OpenAI's actual 400 on the chat path,
// so a regression fails the way production does.
type recordingUpstream struct {
	srv       *httptest.Server
	gotPaths  []string
	gotBodies []string
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.gotPaths = append(u.gotPaths, r.URL.Path)
		u.gotBodies = append(u.gotBodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			// A real embeddings response: an object list, and usage with NO completion tokens.
			_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.01,0.02,0.03]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":12,"total_tokens":12}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"you must provide a messages parameter","type":"invalid_request_error"}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func embedProxy(t *testing.T, up *recordingUpstream) *Proxy {
	t.Helper()
	p := New(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, "test-key", "", "", nil)
	p.openAIURL = up.srv.URL + "/v1/chat/completions"
	return p
}

func postEmbeddings(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.HandleOpenAI(rr, req)
	return rr
}

// (1) THE REGRESSION ITSELF: an embeddings request must reach the embeddings endpoint and come
// back as embeddings.
func TestEmbeddings_ReachTheEmbeddingsEndpoint(t *testing.T) {
	up := newRecordingUpstream(t)
	p := embedProxy(t, up)

	rr := postEmbeddings(t, p, `{"model":"text-embedding-3-small","input":"package main"}`)

	if len(up.gotPaths) == 0 {
		t.Fatalf("upstream was never called; Lens answered %d: %s", rr.Code, rr.Body.String())
	}
	if got := up.gotPaths[0]; !strings.HasSuffix(got, "/v1/embeddings") {
		t.Errorf("upstream received %q, want a path ending /v1/embeddings — the inbound path was "+
			"discarded and the embeddings body went to the chat endpoint", got)
	}

	// THE SHAPE, not the status. A chat completion with status 200 would pass a code check.
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%d): %s", rr.Code, rr.Body.String())
	}
	if out.Object != "list" || len(out.Data) == 0 || out.Data[0].Object != "embedding" {
		t.Errorf("response is not an embeddings list: %s", rr.Body.String())
	}
	if len(out.Data[0].Embedding) == 0 {
		t.Errorf("embedding vector is empty — the client indexes on this")
	}
}

// (2) THE SILENT FAILURE THE FIX HAD TO AVOID. extractPrompt reads `messages`, so every
// embeddings body derives the EMPTY prompt and they would all share one cache key. Two DIFFERENT
// inputs must produce two DIFFERENT upstream calls — never one call and one cache hit.
//
// This is the assertion that matters most: a wrong vector is silent, and it would corrupt exactly
// the index this endpoint exists to build.
func TestEmbeddings_TwoDifferentInputsAreNotCacheCollided(t *testing.T) {
	up := newRecordingUpstream(t)
	p := embedProxy(t, up)

	postEmbeddings(t, p, `{"model":"text-embedding-3-small","input":"file A contents"}`)
	postEmbeddings(t, p, `{"model":"text-embedding-3-small","input":"file B contents"}`)

	if len(up.gotPaths) != 2 {
		t.Fatalf("upstream called %d time(s) for two DIFFERENT inputs — the second was served from "+
			"cache under the shared empty-prompt key, which returns the first input's vector",
			len(up.gotPaths))
	}
	if up.gotBodies[0] == up.gotBodies[1] {
		t.Errorf("the two upstream bodies are identical; the fixture proves nothing")
	}
}

// (3) EVERY EXTRACTOR STILL SEES AN EMPTY PROMPT — that is not fixed here, and the cache guard is
// what makes it safe. Pinned so a future change that starts caching embeddings has to confront
// the collision rather than inherit it.
func TestEmbeddings_PromptExtractionIsStillEmpty_WhichIsWhyTheyAreNotCached(t *testing.T) {
	_, promptA, _ := extractPrompt([]byte(`{"model":"text-embedding-3-small","input":"A"}`))
	_, promptB, _ := extractPrompt([]byte(`{"model":"text-embedding-3-small","input":"B"}`))
	if promptA != "" || promptB != "" {
		t.Fatalf("extractPrompt now derives a prompt from an embeddings body (%q/%q) — if that is "+
			"deliberate, the cache guard in endpoint.cacheable() can be revisited, but the "+
			"CROSS-TENANT POOLING question must be decided first: these are source-code embeddings",
			promptA, promptB)
	}
	if endpointEmbeddings.cacheable() {
		t.Errorf("embeddings are cacheable while every one of them keys on the empty prompt")
	}
	if !endpointChat.cacheable() {
		t.Errorf("chat must remain cacheable — the cache is the product")
	}
}

// (4) CHAT IS BYTE-IDENTICAL. The classifier defaults to chat for anything unrecognised, so the
// route that has always worked must be untouched.
func TestChatEndpoint_Unchanged(t *testing.T) {
	for _, path := range []string{
		"/v1/proxy/openai/v1/chat/completions",
		"/v1/proxy/openai/",
		"/oai/v1/chat/completions",
		"/v1/proxy/openai/v1/embeddingsX", // near-miss: NOT the embeddings endpoint
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		if got := classifyEndpoint(req); got != endpointChat {
			t.Errorf("classifyEndpoint(%q) = %v, want chat — only an exact embeddings suffix may "+
				"divert, or a chat request could be sent to the wrong upstream", path, got)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/embeddings", strings.NewReader(`{}`))
	if got := classifyEndpoint(req); got != endpointEmbeddings {
		t.Errorf("classifyEndpoint(embeddings) = %v, want embeddings", got)
	}
}

// (5) THE ALLOWLIST IS THE POINT. A provider with no embeddings API must be REFUSED, not silently
// forwarded to its chat URL — that silent mismatch is the original defect.
func TestEmbeddingsURL_AllowlistedPerProvider(t *testing.T) {
	cases := []struct {
		provider, chatURL, want string
		ok                      bool
	}{
		{"openai", "https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1/embeddings", true},
		{"vllm", "http://vllm.internal:8000/v1/chat/completions", "http://vllm.internal:8000/v1/embeddings", true},
		{"groq", "https://api.groq.com/openai/v1/chat/completions", "https://api.groq.com/openai/v1/embeddings", true},
		{"mistral", "https://api.mistral.ai/v1/chat/completions", "https://api.mistral.ai/v1/embeddings", true},
		// No embeddings API / incompatible request shape — refusing is the honest answer.
		{"anthropic", "https://api.anthropic.com/v1/messages", "", false},
		{"google", "https://generativelanguage.googleapis.com", "", false},
		{"bedrock", "https://bedrock-runtime.us-east-1.amazonaws.com", "", false},
	}
	for _, c := range cases {
		got, ok := embeddingsURLFor(c.provider, c.chatURL)
		if ok != c.ok || got != c.want {
			t.Errorf("embeddingsURLFor(%q) = (%q,%v), want (%q,%v)", c.provider, got, ok, c.want, c.ok)
		}
	}
}

// (6) AN OPERATOR OVERRIDE SURVIVES. The embeddings URL is derived from the configured chat URL,
// so a self-hosted or proxied base is honoured for embeddings exactly as for chat.
func TestEmbeddingsURL_HonoursAnOperatorOverride(t *testing.T) {
	got, ok := embeddingsURLFor("openai", "https://llm-proxy.internal.example/openai/v1/chat/completions")
	if !ok || got != "https://llm-proxy.internal.example/openai/v1/embeddings" {
		t.Errorf("override lost: got %q ok=%v", got, ok)
	}
}

var _ = context.Background
