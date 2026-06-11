package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIEmbedder_ValidResponseReturnsVector(t *testing.T) {
	var gotReq struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.1,0.2,0.3]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small", srv.URL)

	got, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	want := []float32{0.1, 0.2, 0.3}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%g, want %g", i, got[i], want[i])
		}
	}

	if gotReq.Model != "text-embedding-3-small" {
		t.Errorf("request model = %q, want %q", gotReq.Model, "text-embedding-3-small")
	}
	if gotReq.Input != "hello world" {
		t.Errorf("request input = %q, want %q", gotReq.Input, "hello world")
	}
}

func TestOpenAIEmbedder_Non200ReturnsErrorWithStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(srv.Close)

	e := NewOpenAIEmbedder("bad-key", "text-embedding-3-small", srv.URL)

	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error should mention status 401, got: %v", err)
	}
	if !strings.Contains(msg, "invalid api key") {
		t.Errorf("error should include response body, got: %v", err)
	}
}

func TestOpenAIEmbedder_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not valid json`)
	}))
	t.Cleanup(srv.Close)

	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small", srv.URL)

	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestOpenAIEmbedder_ContextCancellationPropagates(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small", srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call

	_, err := e.Embed(ctx, "hello")
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("server should not be hit on a cancelled context, got %d hits", hits)
	}
}

// TestNewOpenAIEmbedder_BaseURLOverrideHonored pins that the PUBLIC constructor
// (what main.go/config use — not the in-package e.baseURL field) can point the
// embed POST at an operator-configured endpoint. Before this param, the
// operator-config path had no way to redirect embeddings to the trial mock —
// the root cause of #159's never-run semantic-isolation proof.
func TestNewOpenAIEmbedder_BaseURLOverrideHonored(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.5]}]}`)
	}))
	t.Cleanup(srv.Close)

	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small", srv.URL)
	if _, err := e.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !hit {
		t.Error("override base URL not honored: the embed POST never reached the test server")
	}
}

// capturingTransport records the request URL and returns a canned embedding, so
// the DEFAULT target can be asserted without a real network call.
type capturingTransport struct{ gotURL string }

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.gotURL = r.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"embedding":[0.1]}]}`)),
		Header:     make(http.Header),
	}, nil
}

// TestNewOpenAIEmbedder_DefaultBaseURLPreserved pins that an empty override is
// byte-identical to before: the embed POST targets the production OpenAI
// endpoint, not "" or a dropped URL.
func TestNewOpenAIEmbedder_DefaultBaseURLPreserved(t *testing.T) {
	e := NewOpenAIEmbedder("test-key", "text-embedding-3-small", "")
	ct := &capturingTransport{}
	e.client = &http.Client{Transport: ct} // in-package: swap transport, no real network

	if _, err := e.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if ct.gotURL != openAIEmbeddingsURL {
		t.Errorf("default embed URL = %q, want %q (empty override must preserve the OpenAI endpoint)", ct.gotURL, openAIEmbeddingsURL)
	}
}
