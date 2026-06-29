package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/talyvor/lens/internal/inference"
)

// BedrockConfig carries AWS credentials + region. The type and the translate/sign helpers moved to
// internal/inference (PR-3b A′); this alias keeps SetBedrockConfig, the proxy field, and HandleBedrock
// unedited.
type BedrockConfig = inference.BedrockConfig

// SetBedrockConfig overlays AWS credentials onto the Proxy. Called from main.go after proxy.New(...); an
// empty config (zero AccessKeyID) keeps HandleBedrock in 503-graceful-degradation mode.
func (p *Proxy) SetBedrockConfig(cfg BedrockConfig) {
	p.bedrockConfig = cfg
}

// HandleBedrock dispatches a chat request to AWS Bedrock via the Anthropic API hosted there. Same overall
// shape as HandleGoogle: build a per-provider providerConfig and hand it to serve(). Auth is SigV4; URL
// routing carries the Bedrock model ID; the response is reverse-translated to OpenAI shape.
func (p *Proxy) HandleBedrock(w http.ResponseWriter, r *http.Request) {
	cfg := p.bedrockConfig
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		writeError(w, http.StatusServiceUnavailable, "AWS credentials not configured")
		return
	}
	region := r.Header.Get("X-Talyvor-Bedrock-Region")
	if region == "" {
		region = cfg.Region
	}
	if region == "" {
		region = "us-east-1"
	}
	cfg.Region = region

	// Read body once so we can validate the model before invoking serve.
	body, err := readLimitedBody(r, maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4MB limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if _, ok := inference.ModelToBedrockID(probe.Model); !ok {
		writeError(w, http.StatusBadRequest, "model not supported on Bedrock: "+probe.Model)
		return
	}

	// Restore the body so serve() reads the same bytes.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	// Persist the resolved region so configForProvider("bedrock") picks up the per-request override.
	p.bedrockConfig = cfg

	// serve() owns the rest — caching, scoring, fallback, spend. configForProvider("bedrock") builds the
	// actual per-attempt closures (URL, SigV4, translate), which now call into internal/inference.
	p.serve(w, r, p.configForProvider("bedrock"))
}
