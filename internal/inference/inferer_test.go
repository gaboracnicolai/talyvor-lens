package inference

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/retry"
)

func TestBuildOpenAIChatRequest(t *testing.T) {
	out, err := buildOpenAIChatRequest("gpt-4o", "hello world")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", got.Model)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hello world" {
		t.Errorf("messages = %+v, want one user/hello world", got.Messages)
	}
}

func TestExtractFirstChoiceContent(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"the answer"}}]}`)
	got, err := extractFirstChoiceContent(body)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got != "the answer" {
		t.Errorf("content = %q, want %q", got, "the answer")
	}

	if _, err := extractFirstChoiceContent([]byte(`{"choices":[]}`)); err == nil {
		t.Error("empty choices must error")
	}
}

// firstModelForProvider returns a seeded model id for the given provider, skipping the test if none exists
// (keeps the test robust against catalog seed changes).
func firstModelForProvider(t *testing.T, provider string) string {
	t.Helper()
	for _, m := range catalog.All() {
		if m.Provider == provider {
			return m.ID
		}
	}
	t.Skipf("no seeded %s model in the catalog", provider)
	return ""
}

func TestProviderInferer_Infer_OpenAIPath(t *testing.T) {
	model := firstModelForProvider(t, "openai")

	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"mock reply"}}]}`)
	}))
	t.Cleanup(srv.Close)

	inf := NewProviderInferer(&http.Client{}, retry.DefaultConfig(), Endpoints{
		OpenAIURL: srv.URL,
		OpenAIKey: "test-openai-key",
	})

	out, err := inf.Infer(context.Background(), model, "ping")
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if out != "mock reply" {
		t.Errorf("Infer returned %q, want %q", out, "mock reply")
	}
	// The configured key flows as Bearer auth, and the request carries the input as a user message.
	if gotAuth != "Bearer test-openai-key" {
		t.Errorf("upstream Authorization = %q, want Bearer test-openai-key", gotAuth)
	}
	if !strings.Contains(gotBody, `"content":"ping"`) {
		t.Errorf("upstream body missing the input: %s", gotBody)
	}
}

func TestProviderInferer_Infer_UnknownModelErrors(t *testing.T) {
	inf := NewProviderInferer(&http.Client{}, retry.DefaultConfig(), Endpoints{})
	if _, err := inf.Infer(context.Background(), "definitely-not-a-real-model-xyz", "x"); err == nil {
		t.Fatal("unknown model must error (no upstream call)")
	}
}
