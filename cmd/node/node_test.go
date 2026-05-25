package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── ParseModels ─────────────────────────────────

func TestParseModels(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"llama3.1", []string{"llama3.1"}},
		{"llama3.1,mistral", []string{"llama3.1", "mistral"}},
		{" llama3 , mistral , ", []string{"llama3", "mistral"}},
	}
	for _, c := range cases {
		got := ParseModels(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("input %q: expected %d models, got %d", c.in, len(c.want), len(got))
		}
		for i, m := range c.want {
			if got[i] != m {
				t.Fatalf("input %q[%d]: expected %q, got %q", c.in, i, m, got[i])
			}
		}
	}
}

// ─── NodeConfig.Validate ─────────────────────────

func TestNodeConfigValidate_CatchesMissingFields(t *testing.T) {
	cfg := NodeConfig{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty config")
	}
	// Fill in everything except NODE_MODELS — should still fail.
	cfg = NodeConfig{
		LensURL: "http://x", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Provider: "ollama", GPUType: "cpu",
		MaxConcurrent: 4, Port: 9090,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for missing models")
	}
	cfg.Models = []string{"llama3"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected pass with full config, got %v", err)
	}
}

func TestNodeConfigValidate_RejectsInvalidProvider(t *testing.T) {
	cfg := NodeConfig{
		LensURL: "http://x", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Provider: "tabnine", GPUType: "cpu",
		Models: []string{"llama3"}, MaxConcurrent: 4, Port: 9090,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid provider")
	}
}

func TestNodeConfigValidate_RejectsInvalidGPU(t *testing.T) {
	cfg := NodeConfig{
		LensURL: "http://x", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", Provider: "ollama", GPUType: "tpu",
		Models: []string{"llama3"}, MaxConcurrent: 4, Port: 9090,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid GPU type")
	}
}

// ─── OllamaProvider ──────────────────────────────

func TestOllamaProvider_HealthReturnsNilOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()
	p := NewOllamaProvider(srv.URL)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("expected healthy, got %v", err)
	}
}

func TestOllamaProvider_HealthErrorsOn500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := NewOllamaProvider(srv.URL).Health(context.Background()); err == nil {
		t.Fatal("expected error for non-200")
	}
}

func TestOllamaProvider_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3"},{"name":"mistral"}]}`))
	}))
	defer srv.Close()
	p := NewOllamaProvider(srv.URL)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0] != "llama3" || models[1] != "mistral" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// ─── VLLMProvider ────────────────────────────────

func TestVLLMProvider_InferCallsCorrectEndpoint(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"content": "ok"}}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3}
		}`))
	}))
	defer srv.Close()
	p := NewVLLMProvider(srv.URL)
	resp, err := p.Infer(context.Background(), InferRequest{
		Model:    "llama3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Infer: %v", err)
	}
	if hitPath != "/v1/chat/completions" {
		t.Fatalf("expected /v1/chat/completions hit, got %q", hitPath)
	}
	if resp.Text != "ok" || resp.InputTokens != 5 || resp.OutputTokens != 3 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.LatencyMs < 0 {
		t.Fatalf("latency should be non-negative, got %d", resp.LatencyMs)
	}
}

// ─── NewProvider factory ─────────────────────────

func TestNewProvider_RejectsUnknown(t *testing.T) {
	if _, err := NewProvider("not-a-thing", "http://x"); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestNewProvider_BuildsKnown(t *testing.T) {
	for _, kind := range []string{"ollama", "vllm", "llamacpp"} {
		p, err := NewProvider(kind, "http://x")
		if err != nil {
			t.Fatalf("expected to build %s, got %v", kind, err)
		}
		if p == nil {
			t.Fatalf("nil provider for %s", kind)
		}
	}
}

// ─── InferenceServer ─────────────────────────────

// stubProvider is a minimal Provider for the HTTP handler tests.
type stubProvider struct {
	healthErr error
	models    []string
	infer     InferResponse
}

func (s *stubProvider) Health(context.Context) error { return s.healthErr }
func (s *stubProvider) ListModels(context.Context) ([]string, error) {
	return s.models, nil
}
func (s *stubProvider) Infer(context.Context, InferRequest) (InferResponse, error) {
	return s.infer, nil
}

func TestInferenceServer_RejectsBadSecret(t *testing.T) {
	srv := NewInferenceServer(&stubProvider{}, "real-secret", NodeConfig{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/inference",
		strings.NewReader(`{"model":"x"}`))
	req.Header.Set("X-Node-Secret", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestInferenceServer_AcceptsCorrectSecret(t *testing.T) {
	stub := &stubProvider{infer: InferResponse{Text: "hi", OutputTokens: 1, LatencyMs: 10}}
	srv := NewInferenceServer(stub, "real-secret", NodeConfig{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/inference",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Node-Secret", "real-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body InferResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Text != "hi" {
		t.Fatalf("expected text 'hi', got %q", body.Text)
	}
}

func TestInferenceServer_HealthReportsActiveCount(t *testing.T) {
	srv := NewInferenceServer(&stubProvider{}, "", NodeConfig{
		Provider: "ollama", GPUType: "rtx4090", Models: []string{"llama3"},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "healthy" {
		t.Fatalf("expected healthy, got %v", out["status"])
	}
	if out["gpu_type"] != "rtx4090" {
		t.Fatalf("expected gpu_type rtx4090, got %v", out["gpu_type"])
	}
}

// ─── Earnings formatter ──────────────────────────

func TestFormatEarnings_ContainsExpectedFields(t *testing.T) {
	state := NodeState{
		NodeID:      "node-abc123",
		WorkspaceID: "ws_x",
		GPUType:     "rtx4090",
		Provider:    "ollama",
		Models:      []string{"llama3"},
		LensURL:     "http://lens",
		NodeURL:     "http://node",
		RegisteredAt: time.Now(),
	}
	earnings := map[string]any{
		"nodes_active":        2.0,
		"tokens_served_total": 12345.0,
		"earned_total":        1.5,
	}
	balance := map[string]any{
		"balance":         5.25,
		"lifetime_earned": 7.0,
	}
	out := FormatEarnings(state, earnings, balance)
	for _, want := range []string{
		"node-abc123", "RTX 4090", "Online", "12,345",
		"1.5000 LENS", "($0.15)",
		"5.2500 LENS", "($0.53)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestFormatStatus_HealthyVsUnhealthy(t *testing.T) {
	state := NodeState{
		NodeID:       "n1",
		Provider:     "ollama",
		GPUType:      "cpu",
		WorkspaceID:  "ws",
		Models:       []string{"llama3"},
		RegisteredAt: time.Now(),
	}
	healthy := FormatStatus(state, true)
	if !strings.Contains(healthy, "healthy") {
		t.Fatal("expected healthy line")
	}
	unhealthy := FormatStatus(state, false)
	if !strings.Contains(unhealthy, "unhealthy") {
		t.Fatal("expected unhealthy line")
	}
}

// ─── NodeState persistence ──────────────────────

func TestNodeState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TALYVOR_NODE_STATE", tmp+"/state.json")
	in := NodeState{
		NodeID:       "node1",
		NodeSecret:   "secret-xyz",
		WorkspaceID:  "ws",
		LensURL:      "http://lens",
		NodeURL:      "http://node",
		Provider:     "ollama",
		GPUType:      "cpu",
		Models:       []string{"llama3"},
		RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveState(in); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if out.NodeID != in.NodeID || out.NodeSecret != in.NodeSecret {
		t.Fatalf("round-trip mismatch: %+v vs %+v", in, out)
	}
	if err := ClearState(); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	out, _ = LoadState()
	if out.NodeID != "" {
		t.Fatal("expected empty state after clear")
	}
}
