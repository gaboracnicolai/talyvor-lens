package embedder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Same reasoning as doc2query/usage_test.go: the thing under test is the `usage.prompt_tokens`
// JSON tag, so the fake serves bytes. prompt_tokens and total_tokens differ here on purpose —
// they are equal in the real API for a single input, which would make a harness that read the
// wrong one indistinguishable from a correct one.
func TestEmbedWithUsage_ReportsThePromptTokensTheAPIReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5,0.25]}],"usage":{"prompt_tokens":13,"total_tokens":99}}`))
	}))
	defer srv.Close()

	e := NewOpenAIEmbedder("k", "text-embedding-3-small", srv.URL)
	vec, tokens, err := e.EmbedWithUsage(context.Background(), "how do I center a div?")
	if err != nil {
		t.Fatalf("EmbedWithUsage: %v", err)
	}
	if len(vec) != 2 {
		t.Errorf("vector = %v, want 2 dimensions — usage without the vector it paid for is not a "+
			"measurement", vec)
	}
	if tokens != 13 {
		t.Errorf("tokens = %d, want 13 (prompt_tokens, not total_tokens=99)", tokens)
	}
}

// Embed keeps its signature and its behaviour; it delegates so there is one request path.
func TestEmbed_StillReturnsTheVectorAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2,3]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`))
	}))
	defer srv.Close()

	vec, err := NewOpenAIEmbedder("k", "m", srv.URL).Embed(context.Background(), "x")
	if err != nil || len(vec) != 3 {
		t.Errorf("Embed = %v, %v; want a 3-dim vector and no error", vec, err)
	}
}
