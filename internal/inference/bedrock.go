package inference

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

// BedrockConfig carries the AWS credentials and region needed to sign Bedrock requests. Moved from
// internal/proxy (PR-3b A′); proxy keeps a `type BedrockConfig = inference.BedrockConfig` alias so
// SetBedrockConfig + the proxy field + HandleBedrock stay unedited.
type BedrockConfig struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// bedrockModelMap is the friendly-name → AWS Bedrock model-id table.
var bedrockModelMap = map[string]string{
	"claude-opus-4-6":   "anthropic.claude-opus-4-6-20251101-v1:0",
	"claude-sonnet-4-6": "anthropic.claude-sonnet-4-6-20251101-v1:0",
	"claude-opus-4-5":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-sonnet-4-5": "anthropic.claude-sonnet-4-5-20251022-v2:0",
	"claude-haiku-4-5":  "anthropic.claude-haiku-4-5-20241022-v1:0",
}

// ModelToBedrockID maps a friendly model name to its AWS Bedrock model id (ok=false when unsupported).
func ModelToBedrockID(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	id, ok := bedrockModelMap[model]
	return id, ok
}

// TranslateToBedrockFormat converts an OpenAI-shaped chat request to the Bedrock Anthropic body (strips
// the model field — Bedrock carries it in the URL — and adds the wire version tag + a max_tokens default).
func TranslateToBedrockFormat(body []byte) ([]byte, error) {
	var src map[string]any
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, fmt.Errorf("bedrock translate: %w", err)
	}
	delete(src, "model")
	src["anthropic_version"] = "bedrock-2023-05-31"
	if _, ok := src["max_tokens"]; !ok {
		src["max_tokens"] = 1024
	}
	return json.Marshal(src)
}

// TranslateFromBedrockFormat reshapes a Bedrock-Anthropic response into the OpenAI chat.completion shape.
func TranslateFromBedrockFormat(body []byte, model string) ([]byte, error) {
	var raw struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Role       string `json:"role"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
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
		"id":     "bedrock-" + model,
		"object": "chat.completion",
		"model":  model,
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

// SignRequest applies AWS Signature Version 4 to req (stdlib crypto only — no AWS SDK dependency). The
// body is buffered to hash it, then restored as a re-readable reader so transport still has the bytes.
func SignRequest(req *http.Request, cfg BedrockConfig) error {
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return errors.New("bedrock: missing AWS credentials")
	}
	region := cfg.Region
	if region == "" {
		return errors.New("bedrock: missing AWS region")
	}

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
