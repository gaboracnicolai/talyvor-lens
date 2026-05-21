package localrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/router"
)

// setupOllama wires a fake /api/tags response and an optional /api/generate
// response. Generate failures are simulated by leaving genResp empty.
func setupOllama(t *testing.T, models []string, genResp string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			ms := make([]map[string]any, 0, len(models))
			for _, n := range models {
				ms = append(ms, map[string]any{"name": n, "size": float64(1.0)})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"models": ms})
		case "/api/generate":
			if genResp == "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, genResp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckAvailability_TrueWhenOllamaResponds(t *testing.T) {
	srv := setupOllama(t, []string{"llama3.2:latest"}, "")
	lr := New(srv.URL)

	if !lr.CheckAvailability(context.Background()) {
		t.Error("expected CheckAvailability=true when Ollama is reachable")
	}
}

func TestCheckAvailability_FalseWhenOllamaDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // immediately stop — the URL points nowhere
	lr := New(srv.URL)

	if lr.CheckAvailability(context.Background()) {
		t.Error("expected CheckAvailability=false when Ollama is unreachable")
	}
}

func TestShouldUseLocal_FalseWhenUnavailable(t *testing.T) {
	lr := New("http://127.0.0.1:0") // never reachable
	// availability not yet checked → false

	decision := lr.ShouldUseLocal(router.RequestComplexity{}, "default")
	if decision.UseLocal {
		t.Errorf("UseLocal=true while unavailable; got %+v", decision)
	}
}

func TestShouldUseLocal_FalseForComplexQueries(t *testing.T) {
	srv := setupOllama(t, []string{"llama3.2:latest"}, "")
	lr := New(srv.URL)
	if !lr.CheckAvailability(context.Background()) {
		t.Fatal("setup: expected available")
	}

	complex := router.RequestComplexity{
		HasCode: true, HasMath: true, HasMultiStep: true,
	} // Score = 3 → too complex
	decision := lr.ShouldUseLocal(complex, "default")
	if decision.UseLocal {
		t.Errorf("UseLocal=true for complex query; got %+v", decision)
	}
}

func TestShouldUseLocal_TrueForSimpleQueryWhenAvailable(t *testing.T) {
	srv := setupOllama(t, []string{"llama3.2:latest", "mistral:latest"}, "")
	lr := New(srv.URL)
	if !lr.CheckAvailability(context.Background()) {
		t.Fatal("setup: expected available")
	}

	simple := router.RequestComplexity{} // Score = 0
	decision := lr.ShouldUseLocal(simple, "default")
	if !decision.UseLocal {
		t.Fatalf("UseLocal=false for simple query; got %+v", decision)
	}
	if !strings.HasPrefix(decision.Model, "llama3.2") {
		t.Errorf("preferred model = %q, want one starting with llama3.2", decision.Model)
	}
	if decision.Reason == "" {
		t.Error("Reason should not be empty")
	}
}

func TestShouldUseLocal_FalseForEnterpriseWorkspace(t *testing.T) {
	srv := setupOllama(t, []string{"llama3.2:latest"}, "")
	lr := New(srv.URL)
	if !lr.CheckAvailability(context.Background()) {
		t.Fatal("setup: expected available")
	}

	decision := lr.ShouldUseLocal(router.RequestComplexity{}, "team-a")
	if decision.UseLocal {
		t.Errorf("UseLocal=true for enterprise workspace %q; got %+v", "team-a", decision)
	}
}

func TestForward_SendsCorrectRequestToOllama(t *testing.T) {
	var gotBody struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":"hello","done":true}`)
	}))
	t.Cleanup(srv.Close)

	lr := New(srv.URL)
	out, err := lr.Forward(context.Background(), "llama3.2", "hello world")
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("expected response body forwarded, got %q", out)
	}
	if gotBody.Model != "llama3.2" {
		t.Errorf("upstream got model=%q, want llama3.2", gotBody.Model)
	}
	if gotBody.Prompt != "hello world" {
		t.Errorf("upstream got prompt=%q, want %q", gotBody.Prompt, "hello world")
	}
	if gotBody.Stream != false {
		t.Errorf("upstream got stream=%v, want false", gotBody.Stream)
	}
}

func TestFormatAsOpenAI_ConvertsOllamaResponseCorrectly(t *testing.T) {
	lr := New("http://unused")
	ollama := []byte(`{"response":"Hello, world!","done":true}`)

	formatted, err := lr.FormatAsOpenAI(ollama, "llama3.2")
	if err != nil {
		t.Fatalf("FormatAsOpenAI: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(formatted, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got["object"])
	}
	if got["model"] != "llama3.2" {
		t.Errorf("model = %v, want llama3.2", got["model"])
	}
	choices, ok := got["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices not 1-element array: %v", got["choices"])
	}
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", msg["role"])
	}
	if msg["content"] != "Hello, world!" {
		t.Errorf("content = %v, want %q", msg["content"], "Hello, world!")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
}

func TestForward_ErrorReturnsErrorSoCallerFallsBackToCloud(t *testing.T) {
	srv := setupOllama(t, []string{"llama3.2:latest"}, "") // generate returns 500
	lr := New(srv.URL)

	_, err := lr.Forward(context.Background(), "llama3.2", "hello")
	if err == nil {
		t.Error("expected error when Ollama returns 500; got nil")
	}
}
