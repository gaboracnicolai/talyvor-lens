package inference

import (
	"context"
	"encoding/json"
	"fmt"
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

// openAIShapeStub answers the way OpenAI does for the shapes B26.10 is about: a GPT-5.x body carrying
// max_tokens or temperature is a 400, /v1/chat/completions answers a -pro or -codex model 404, and
// /v1/responses takes `input` (never `messages`) and answers a Responses object.
func openAIShapeStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			fail(http.StatusBadRequest, "We could not parse the JSON body of your request.")
			return
		}
		model, _ := b["model"].(string)
		if strings.HasPrefix(model, "gpt-5") {
			for _, k := range []string{"max_tokens", "temperature"} {
				if _, ok := b[k]; ok {
					fail(http.StatusBadRequest, "Unsupported parameter: '"+k+"' is not supported with this model.")
					return
				}
			}
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			if strings.Contains(model, "-pro") || strings.Contains(model, "-codex") {
				fail(http.StatusNotFound, "This model is only supported in v1/responses and not in v1/chat/completions.")
				return
			}
			if _, ok := b["messages"]; !ok {
				fail(http.StatusBadRequest, "Missing required parameter: 'messages'.")
				return
			}
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"chat:%s"}}]}`, model)
		case "/v1/responses":
			if _, ok := b["messages"]; ok {
				fail(http.StatusBadRequest, "Unsupported parameter: 'messages'.")
				return
			}
			if input, _ := b["input"].([]any); len(input) == 0 {
				fail(http.StatusBadRequest, "Missing required parameter: 'input'.")
				return
			}
			fmt.Fprintf(w, `{"id":"resp_1","object":"response","model":%q,"status":"completed","output":[`+
				`{"type":"reasoning","summary":[]},`+
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses:%s"}]}]}`, model, model)
		default:
			fail(http.StatusNotFound, "no route "+r.URL.Path)
		}
	}))
}

// B26.10: through the helper a GPT-5.x, a -pro and a -codex model each answer — the -pro and -codex
// ones over /v1/responses, which is the only endpoint OpenAI serves them on.
func TestProviderInferer_Infer_GPT5ProAndCodex(t *testing.T) {
	srv := openAIShapeStub()
	t.Cleanup(srv.Close)
	inf := NewProviderInferer(&http.Client{}, retry.DefaultConfig(), Endpoints{
		OpenAIURL: srv.URL + "/v1/chat/completions",
		OpenAIKey: "test-openai-key",
	})
	for _, tc := range []struct{ model, want string }{
		{"gpt-5.5", "chat:gpt-5.5"},
		{"gpt-5.4-pro", "responses:gpt-5.4-pro"},
		{"gpt-5.3-codex", "responses:gpt-5.3-codex"},
	} {
		if _, ok := catalog.Get(tc.model); !ok {
			t.Fatalf("%s is not in the catalog", tc.model)
		}
		got, err := inf.Infer(context.Background(), tc.model, "ping")
		if err != nil || got != tc.want {
			t.Errorf("Infer(%s) = %q, %v; want %q", tc.model, got, err, tc.want)
		}
	}
}
