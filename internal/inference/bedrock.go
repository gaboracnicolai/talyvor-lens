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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/talyvor/lens/internal/catalog"
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

// bedrockModelMap is the friendly-name → AWS Bedrock model-id table. The 4.6 ids are AWS's, from the
// model cards' "Programmatic Access" tables (B23.7, fetched 2026-09-29).
//
// B26.11 — the Sonnet and Haiku 4.5 ids are AWS's too (checked 2026-10-03; they were
// anthropic.claude-sonnet-4-5-20251022-v2:0 and anthropic.claude-haiku-4-5-20241022-v1:0, which match
// no AWS model). docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-sonnet-4-5.html
// gives anthropic.claude-sonnet-4-5-20250929-v1:0. …/model-card-anthropic-claude-haiku-4-5.html gives
// no bare bedrock-runtime id ("requires a geo or global inference profile ID"), so Haiku 4.5 is listed
// by its global profile here and BedrockModelIDForRegion swaps in the geo profile for the Region (B27.10).
var bedrockModelMap = map[string]string{
	"claude-opus-4-6":   "anthropic.claude-opus-4-6-v1",
	"claude-sonnet-4-6": "anthropic.claude-sonnet-4-6",
	"claude-opus-4-5":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-sonnet-4-5": "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-haiku-4-5":  "global.anthropic.claude-haiku-4-5-20251001-v1:0",
}

// ModelToBedrockID maps a friendly model name to its AWS Bedrock model id (ok=false when unsupported).
// A catalog Bedrock id is accepted too — the canonical one, or an old id kept as its alias — and is
// sent to AWS as the canonical id.
func ModelToBedrockID(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	if id, ok := bedrockModelMap[model]; ok {
		return id, true
	}
	if m, ok := catalog.Get(model); ok && m.Provider == "bedrock" {
		return m.ID, true
	}
	return "", false
}

// bedrockGeoPrefix — B27.10. A global inference profile can route a request to any Region in the world,
// so for a model Lens sends by its global profile, each Bedrock source Region maps to the geo profile
// (us., eu., au., jp., in.) whose source Regions include it. That keeps the request inside the
// operator's geography. A Region missing here has no geo profile and stays on global.
//
// From AWS's "Geo inference details" tables at
// docs.aws.amazon.com/bedrock/latest/userguide/model-card-anthropic-claude-haiku-4-5.html (fetched
// 2026-10-04). ca-central-1 is a source Region of the US profile on that page.
var bedrockGeoPrefix = map[string]map[string]string{
	"global.anthropic.claude-haiku-4-5-20251001-v1:0": {
		"us-east-1": "us", "us-east-2": "us", "us-west-1": "us", "us-west-2": "us", "ca-central-1": "us",
		"eu-central-1": "eu", "eu-central-2": "eu", "eu-north-1": "eu", "eu-south-1": "eu",
		"eu-south-2": "eu", "eu-west-1": "eu", "eu-west-2": "eu", "eu-west-3": "eu",
		"ap-southeast-2": "au", "ap-southeast-4": "au", "ap-southeast-6": "au",
		"ap-northeast-1": "jp", "ap-northeast-3": "jp",
		"ap-south-1": "in", "ap-south-2": "in",
	},
}

// globalOnlyLogged remembers which (Region, model id) pairs already logged that they go by the global
// profile, so the line appears once per process rather than on every request.
var globalOnlyLogged sync.Map

// BedrockModelIDForRegion is ModelToBedrockID for a request signed for region: a global inference
// profile id becomes the geo profile containing region, and stays global (with a log line saying so)
// only where AWS publishes no geo profile for that Region.
func BedrockModelIDForRegion(model, region string) (string, bool) {
	id, ok := ModelToBedrockID(model)
	if !ok {
		return "", false
	}
	rest, isGlobal := strings.CutPrefix(id, "global.")
	if !isGlobal {
		return id, true
	}
	if geo, ok := bedrockGeoPrefix[id][region]; ok {
		return geo + "." + rest, true
	}
	if _, seen := globalOnlyLogged.LoadOrStore(region+"|"+id, struct{}{}); !seen {
		slog.Warn("bedrock: no geo inference profile for this Region, using the global profile — requests can be served outside the Region's geography",
			slog.String("region", region), slog.String("model_id", id))
	}
	return id, true
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
