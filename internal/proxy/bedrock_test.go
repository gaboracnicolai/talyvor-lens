package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

func TestModelToBedrockID_MapsClaudeSonnet46Correctly(t *testing.T) {
	id, ok := modelToBedrockID("claude-sonnet-4-6")
	if !ok {
		t.Fatal("claude-sonnet-4-6 should be supported")
	}
	if id != "anthropic.claude-sonnet-4-6-20251101-v1:0" {
		t.Errorf("got %q, want anthropic.claude-sonnet-4-6-20251101-v1:0", id)
	}
}

func TestModelToBedrockID_UnknownModelReturnsFalse(t *testing.T) {
	if _, ok := modelToBedrockID("gpt-4"); ok {
		t.Error("gpt-4 should NOT be supported on Bedrock")
	}
	if _, ok := modelToBedrockID("gemini-2.5-pro"); ok {
		t.Error("gemini-2.5-pro should NOT be supported on Bedrock")
	}
	if _, ok := modelToBedrockID(""); ok {
		t.Error("empty model must not map")
	}
}

func TestTranslateToBedrockFormat_RemovesModelField(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateToBedrockFormat(in)
	if err != nil {
		t.Fatalf("translateToBedrockFormat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := got["model"]; present {
		t.Errorf("Bedrock body still contains 'model' field: %s", out)
	}
}

func TestTranslateToBedrockFormat_AddsAnthropicVersion(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	out, _ := translateToBedrockFormat(in)
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("anthropic_version = %v, want bedrock-2023-05-31", got["anthropic_version"])
	}
}

func TestTranslateFromBedrockFormat_ReturnsOpenAIShape(t *testing.T) {
	in := []byte(`{"content":[{"type":"text","text":"hi from claude"}],"role":"assistant"}`)
	out, err := translateFromBedrockFormat(in, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("translateFromBedrockFormat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion (client should see OpenAI shape)", got["object"])
	}
	choices, _ := got["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in translated response: %v", got)
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if !strings.Contains(msg["content"].(string), "hi from claude") {
		t.Errorf("content lost in translation: %v", msg["content"])
	}
}

func TestSignRequest_AddsAuthorizationHeader(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "AKIA-TEST", SecretAccessKey: "SECRET",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-sonnet-4-6-20251101-v1:0/invoke",
		strings.NewReader(`{"messages":[]}`))
	if err := signRequest(req, cfg); err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization missing SigV4 prefix: %q", auth)
	}
	if !strings.Contains(auth, "Credential=AKIA-TEST/") {
		t.Errorf("Authorization missing credential: %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-date") {
		t.Errorf("Authorization missing signed headers: %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization missing signature: %q", auth)
	}
}

func TestSignRequest_AddsAmzDateHeader(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "k", SecretAccessKey: "s",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke",
		strings.NewReader(`{}`))
	_ = signRequest(req, cfg)
	dt := req.Header.Get("X-Amz-Date")
	if dt == "" {
		dt = req.Header.Get("x-amz-date")
	}
	if dt == "" {
		t.Fatal("x-amz-date header missing")
	}
	if len(dt) != 16 || !strings.HasSuffix(dt, "Z") {
		t.Errorf("x-amz-date = %q, want ISO8601 basic format YYYYMMDDTHHMMSSZ", dt)
	}
}

func TestSignRequest_AddsSessionTokenWhenSet(t *testing.T) {
	cfg := BedrockConfig{
		Region: "us-east-1", AccessKeyID: "k", SecretAccessKey: "s",
		SessionToken: "fake-temp-token",
	}
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/m/invoke",
		strings.NewReader(`{}`))
	_ = signRequest(req, cfg)
	if got := req.Header.Get("X-Amz-Security-Token"); got != "fake-temp-token" {
		t.Errorf("x-amz-security-token = %q, want fake-temp-token", got)
	}
}

// newBedrockProxy is a test helper that builds a Proxy with Bedrock
// credentials configured and a swappable bedrockURL pointing at the
// caller's httptest server.
func newBedrockProxy(t *testing.T, bedrockURL string, withCreds bool) *Proxy {
	t.Helper()
	exact, _ := newExactCacheForTest(t)
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	if withCreds {
		p.SetBedrockConfig(BedrockConfig{
			Region: "us-east-1", AccessKeyID: "AKIA-TEST", SecretAccessKey: "SECRET",
		})
	}
	p.bedrockURL = bedrockURL
	return p
}

func TestHandleBedrock_Returns503WhenNoAWSCredentials(t *testing.T) {
	p := newBedrockProxy(t, "", false)
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/bedrock/invoke", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.HandleBedrock(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleBedrock_Returns400ForUnsupportedModel(t *testing.T) {
	p := newBedrockProxy(t, "http://example.invalid", true)
	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/bedrock/invoke", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.HandleBedrock(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not supported") && !strings.Contains(w.Body.String(), "model") {
		t.Errorf("400 body should mention unsupported model: %s", w.Body.String())
	}
}

func TestHandleBedrock_ForwardsCorrectlyToMockBedrock(t *testing.T) {
	var (
		gotPath  string
		gotAuth  string
		gotBody  []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hello from bedrock"}],"role":"assistant"}`)
	}))
	t.Cleanup(srv.Close)

	p := newBedrockProxy(t, srv.URL, true)
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/bedrock/invoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.HandleBedrock(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotPath, "anthropic.claude-sonnet-4-6-20251101-v1:0") {
		t.Errorf("upstream path missing Bedrock model id; got %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("upstream did not see SigV4 Authorization; got %q", gotAuth)
	}
	// Body should be Bedrock-shaped (anthropic_version present, no model field).
	var sent map[string]any
	_ = json.Unmarshal(gotBody, &sent)
	if sent["anthropic_version"] != "bedrock-2023-05-31" {
		t.Errorf("upstream body missing anthropic_version: %s", gotBody)
	}
	if _, present := sent["model"]; present {
		t.Errorf("upstream body still has model field: %s", gotBody)
	}
	// Client response is OpenAI-shaped.
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object"] != "chat.completion" {
		t.Errorf("client did not see OpenAI shape: %v", resp)
	}
}
