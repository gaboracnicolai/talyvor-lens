package proxy

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
)

func newExtraProxy(t *testing.T) *Proxy {
	t.Helper()
	exact, _ := newExactCacheForTest(t)
	return New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
}

func mockOpenAIShape(t *testing.T, hits *int, saw *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		if saw != nil {
			*saw = r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleExtraProvider_MistralForwardsCorrectly(t *testing.T) {
	var hits int
	var sawAuth string
	srv := mockOpenAIShape(t, &hits, &sawAuth)

	p := newExtraProxy(t)
	p.SetExtraProviderConfig(ExtraProviderConfig{MistralKey: "mistral-test"})
	p.mistralURL = srv.URL

	body := `{"model":"mistral-large-latest","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/mistral/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleExtraProvider("mistral")(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if hits == 0 {
		t.Error("upstream not hit")
	}
	if sawAuth != "Bearer mistral-test" {
		t.Errorf("upstream saw Authorization=%q, want Bearer mistral-test", sawAuth)
	}
}

func TestHandleExtraProvider_GroqForwardsCorrectly(t *testing.T) {
	var hits int
	var sawAuth string
	srv := mockOpenAIShape(t, &hits, &sawAuth)

	p := newExtraProxy(t)
	p.SetExtraProviderConfig(ExtraProviderConfig{GroqKey: "groq-test"})
	p.groqURL = srv.URL

	body := `{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/groq/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleExtraProvider("groq")(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if hits == 0 {
		t.Error("upstream not hit")
	}
	if sawAuth != "Bearer groq-test" {
		t.Errorf("upstream saw Authorization=%q, want Bearer groq-test", sawAuth)
	}
}

func TestHandleExtraProvider_VLLMUsesConfiguredBaseURL(t *testing.T) {
	var hits int
	srv := mockOpenAIShape(t, &hits, nil)

	p := newExtraProxy(t)
	// vLLM without API key — auth header should be omitted.
	p.SetExtraProviderConfig(ExtraProviderConfig{VLLMURL: srv.URL})

	body := `{"model":"llama-3-private","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/vllm/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	p.HandleExtraProvider("vllm")(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if hits == 0 {
		t.Error("vLLM upstream not hit")
	}
}

func TestHandleExtraProvider_Returns503ForMissingKey(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		cfg      ExtraProviderConfig
	}{
		{"mistral no key", "mistral", ExtraProviderConfig{}},
		{"groq no key", "groq", ExtraProviderConfig{}},
		{"vllm no url", "vllm", ExtraProviderConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newExtraProxy(t)
			p.SetExtraProviderConfig(tc.cfg)

			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/proxy/"+tc.provider+"/x", strings.NewReader(body))
			w := httptest.NewRecorder()
			p.HandleExtraProvider(tc.provider)(w, req)

			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestHandleExtraProvider_Returns400ForUnknownProvider(t *testing.T) {
	p := newExtraProxy(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/nopenope/x", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.HandleExtraProvider("nopenope")(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestPricing_MistralModelsCorrect(t *testing.T) {
	// mistral-large-latest: $2/M input, $6/M output → 1M+1M = $8.00.
	if got := alerts.CostUSD("mistral-large-latest", 1_000_000, 1_000_000); math.Abs(got-8.00) > 1e-9 {
		t.Errorf("mistral-large 1M+1M = %v, want 8.00", got)
	}
	// mistral-small-latest: $0.10/M input, $0.30/M output → 1M+1M = $0.40.
	if got := alerts.CostUSD("mistral-small-latest", 1_000_000, 1_000_000); math.Abs(got-0.40) > 1e-9 {
		t.Errorf("mistral-small 1M+1M = %v, want 0.40", got)
	}
}

func TestPricing_GroqModelsCorrect(t *testing.T) {
	// llama-3.3-70b-versatile: $0.59 input, $0.79 output → 1M+1M = $1.38.
	if got := alerts.CostUSD("llama-3.3-70b-versatile", 1_000_000, 1_000_000); math.Abs(got-1.38) > 1e-9 {
		t.Errorf("llama-3.3-70b 1M+1M = %v, want 1.38", got)
	}
	// llama-3.1-8b-instant: $0.05 input, $0.08 output → 1M+1M = $0.13.
	if got := alerts.CostUSD("llama-3.1-8b-instant", 1_000_000, 1_000_000); math.Abs(got-0.13) > 1e-9 {
		t.Errorf("llama-3.1-8b 1M+1M = %v, want 0.13", got)
	}
}

func TestPricing_VLLMCostsZero(t *testing.T) {
	// vLLM is self-hosted: any vllm/* model costs zero.
	if got := alerts.CostUSD("vllm/llama-3-70b", 1_000_000, 1_000_000); got != 0 {
		t.Errorf("vllm/* should cost $0 (self-hosted); got %v", got)
	}
	if got := alerts.CostUSD("vllm/anything", 999_999, 999_999); got != 0 {
		t.Errorf("vllm/* should cost $0 (self-hosted); got %v", got)
	}
}
