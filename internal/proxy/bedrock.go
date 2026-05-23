package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BedrockConfig carries the AWS credentials and region needed to sign
// Bedrock requests. Kept separate from cfg.* so the proxy can hold a
// concrete value without dragging the wider config package into every
// test.
type BedrockConfig struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// bedrockModelMap is the friendly-name → AWS Bedrock model-id table.
// Keep sorted by family so future model bumps are easy to read.
var bedrockModelMap = map[string]string{
	"claude-opus-4-6":   "anthropic.claude-opus-4-6-20251101-v1:0",
	"claude-sonnet-4-6": "anthropic.claude-sonnet-4-6-20251101-v1:0",
	"claude-haiku-4-6":  "anthropic.claude-haiku-4-6-20251103-v1:0",
	"claude-opus-4-5":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-sonnet-4-5": "anthropic.claude-sonnet-4-5-20251022-v2:0",
	"claude-haiku-4-5":  "anthropic.claude-haiku-4-5-20241022-v1:0",
}

func modelToBedrockID(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	id, ok := bedrockModelMap[model]
	return id, ok
}

// translateToBedrockFormat converts an OpenAI-shaped chat request to
// the Bedrock Anthropic body. The model field is stripped — Bedrock
// carries the model ID in the URL path, not the body — and the wire
// version tag is added so AWS knows which protocol to use.
func translateToBedrockFormat(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("bedrock translate: %w", err)
	}
	delete(src, "model")
	src["anthropic_version"] = "bedrock-2023-05-31"
	if _, ok := src["max_tokens"]; !ok {
		// Bedrock requires max_tokens. 1024 matches the proxy's other
		// defaults and avoids surprising the caller.
		src["max_tokens"] = 1024
	}
	return json.Marshal(src)
}

// translateFromBedrockFormat reshapes a Bedrock-Anthropic response into
// the OpenAI chat.completion shape so downstream caching, scoring, and
// spend code don't need to know which provider answered.
func translateFromBedrockFormat(body []byte, model string) ([]byte, error) {
	var raw struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Role         string `json:"role"`
		StopReason   string `json:"stop_reason"`
		Usage        struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("bedrock decode: %w", err)
	}
	var sb strings.Builder
	for _, c := range raw.Content {
		if c.Type == "text" || c.Type == "" {
			sb.WriteString(c.Text)
		}
	}
	finishReason := "stop"
	if raw.StopReason == "max_tokens" {
		finishReason = "length"
	}
	out := map[string]any{
		"id":      "bedrock-" + model,
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": sb.String(),
				},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     raw.Usage.InputTokens,
			"completion_tokens": raw.Usage.OutputTokens,
			"total_tokens":      raw.Usage.InputTokens + raw.Usage.OutputTokens,
		},
	}
	return json.Marshal(out)
}

// signRequest applies AWS Signature Version 4 to req. The request body
// is fully buffered so the SHA256 of the payload can be computed; the
// body is then replaced with a re-readable bytes.Reader so the actual
// HTTP transport still has the bytes to send.
//
// Implemented against stdlib crypto/hmac + crypto/sha256 only — the
// AWS SDK is intentionally not a dependency.
func signRequest(req *http.Request, cfg BedrockConfig) error {
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return errors.New("bedrock: missing AWS credentials")
	}
	region := cfg.Region
	if region == "" {
		return errors.New("bedrock: missing AWS region")
	}

	// Read + restore body so we can hash it for the canonical request.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("bedrock: read body: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	service := "bedrock"
	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"

	host := req.URL.Host
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Date", amzDate)
	if cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", cfg.SessionToken)
	}

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQuery := req.URL.Query().Encode()

	// SignedHeaders is the minimum required set — host + x-amz-date.
	// Including the session token here would be more compliant in some
	// SDKs but Bedrock accepts the minimal form, which keeps the canonical
	// request deterministic regardless of whether STS creds are in play.
	signedHeaders := "host;x-amz-date"
	canonicalHeaders := "host:" + strings.ToLower(host) + "\n" +
		"x-amz-date:" + amzDate + "\n"

	payloadHash := sha256Hex(body)

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	dateKey := hmacSHA256([]byte("AWS4"+cfg.SecretAccessKey), []byte(dateStamp))
	dateRegionKey := hmacSHA256(dateKey, []byte(region))
	dateRegionServiceKey := hmacSHA256(dateRegionKey, []byte(service))
	signingKey := hmacSHA256(dateRegionServiceKey, []byte("aws4_request"))

	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	auth := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.AccessKeyID, scope, signedHeaders, signature)
	req.Header.Set("Authorization", auth)
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// SetBedrockConfig overlays AWS credentials onto the Proxy. Called from
// main.go after proxy.New(...) so we don't have to extend New's already-
// long signature; an empty config (zero AccessKeyID) keeps HandleBedrock
// in 503-graceful-degradation mode.
func (p *Proxy) SetBedrockConfig(cfg BedrockConfig) {
	p.bedrockConfig = cfg
}

// HandleBedrock dispatches a chat request to AWS Bedrock via the
// Anthropic API hosted there. Same overall shape as HandleGoogle:
// build a per-provider providerConfig and hand it to serve(). Auth is
// SigV4; URL routing carries the Bedrock model ID rather than the
// friendly name; the response is reverse-translated to OpenAI shape.
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
	if _, ok := modelToBedrockID(probe.Model); !ok {
		writeError(w, http.StatusBadRequest, "model not supported on Bedrock: "+probe.Model)
		return
	}

	// Restore the body so serve() reads the same bytes.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	// Persist the resolved region so configForProvider("bedrock") picks
	// up the per-request override. SetBedrockConfig is the production
	// path; this overlay supports the X-Talyvor-Bedrock-Region header.
	p.bedrockConfig = cfg

	// serve() owns the rest of the flow — caching, scoring, fallback,
	// spend. configForProvider("bedrock") in proxy.go builds the actual
	// per-attempt closures (URL, SigV4, translate).
	p.serve(w, r, p.configForProvider("bedrock"))
}
