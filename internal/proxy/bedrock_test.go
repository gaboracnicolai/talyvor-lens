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

// NOTE (PR-3b A′): the bedrock UNIT tests (TestModelToBedrockID_*, TestTranslateToBedrockFormat_*,
// TestTranslateFromBedrockFormat_*, TestSignRequest_*) moved to internal/inference with their funcs. The
// handler-level TestHandleBedrock_* tests below stay here, byte-identical.

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
		gotPath string
		gotAuth string
		gotBody []byte
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
