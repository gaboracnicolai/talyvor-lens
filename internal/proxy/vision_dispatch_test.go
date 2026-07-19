package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/distill"
)

// canned anthropic messages response carrying transcribed text + reported usage.
func anthropicVisionResp(text string, in, out int) []byte {
	m := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
		"usage": map[string]any{"input_tokens": in, "output_tokens": out},
	}
	b, _ := json.Marshal(m)
	return b
}

func visionReq() distill.VisionRequest {
	return distill.VisionRequest{
		Bytes:     []byte("%PDF-scanned-bytes"),
		MediaType: "application/pdf",
		Format:    distill.FormatPDF,
		Prompt:    distill.DefaultVisionPrompt,
	}
}

// A NeedsVision document on an Anthropic workspace is OCR'd: the dispatcher
// selects a document-capable model, builds a base64 document block, forwards it,
// and returns the transcribed text + the provider's REPORTED token cost.
func TestVisionDispatcher_AnthropicSuccess(t *testing.T) {
	var gotBody []byte
	var gotModel string
	d := &visionDispatcher{
		provider:      "anthropic",
		allowedModels: nil, // empty allow-list = any capable model
		maxTokens:     4096,
		forward: func(_ context.Context, body []byte, model string) ([]byte, int, error) {
			gotBody, gotModel = body, model
			return anthropicVisionResp("# Scanned\n\nrecovered text", 1200, 50), 200, nil
		},
	}

	res, err := d.DispatchVision(context.Background(), visionReq())
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.Markdown != "# Scanned\n\nrecovered text" {
		t.Errorf("markdown = %q", res.Markdown)
	}
	if res.InputTokens != 1200 || res.OutputTokens != 50 {
		t.Errorf("token cost from reported usage wrong: in=%d out=%d", res.InputTokens, res.OutputTokens)
	}
	if res.Model != "claude-haiku-4-5" || gotModel != "claude-haiku-4-5" {
		t.Errorf("expected the preferred document-capable model; res=%q forwarded=%q", res.Model, gotModel)
	}

	// The forwarded body must be a valid Anthropic document request: the model,
	// max_tokens, a base64 document block of the ORIGINAL bytes, and the prompt.
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("forwarded body is not valid JSON: %v", err)
	}
	if sent.Model != "claude-haiku-4-5" || sent.MaxTokens != 4096 {
		t.Errorf("request envelope wrong: model=%q max_tokens=%d", sent.Model, sent.MaxTokens)
	}
	if len(sent.Messages) != 1 || len(sent.Messages[0].Content) != 2 {
		t.Fatalf("expected one user message with a document + text block; got %+v", sent.Messages)
	}
	doc := sent.Messages[0].Content[0]
	if doc.Type != "document" || doc.Source.Type != "base64" || doc.Source.MediaType != "application/pdf" {
		t.Errorf("document block wrong: %+v", doc)
	}
	if raw, _ := base64.StdEncoding.DecodeString(doc.Source.Data); string(raw) != "%PDF-scanned-bytes" {
		t.Errorf("document data must be the original bytes, base64-encoded; got %q", string(raw))
	}
	if txt := sent.Messages[0].Content[1]; txt.Type != "text" || txt.Text != distill.DefaultVisionPrompt {
		t.Errorf("prompt block wrong: %+v", txt)
	}
}

// A provider with no document-capable model in the allow-list (e.g. OpenAI)
// yields an error — the orchestrator's visionFallback then passes the original
// request through unchanged.
func TestVisionDispatcher_NoCapableModel(t *testing.T) {
	d := &visionDispatcher{
		provider:  "openai", // no document-capable model in the catalog
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			t.Fatal("forward must NOT be called when no capable model exists")
			return nil, 0, nil
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("expected an error for a provider with no document-capable model")
	}
}

// Google IS document-capable per the catalog, but the proxy's Gemini translation
// drops document blocks (flattens to text) — sending OCR there would make the
// model hallucinate on an empty prompt. The dispatcher must REFUSE rather than
// silently produce fake "OCR".
func TestVisionDispatcher_UnsupportedRequestPathRefused(t *testing.T) {
	d := &visionDispatcher{
		provider:  "google",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			t.Fatal("forward must NOT be called for a provider whose request path drops documents")
			return nil, 0, nil
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("expected a refusal for a provider that cannot carry a document block")
	}
}

// Respect the workspace allow-list: when only a specific capable model is
// allowed, that model is selected (not the global preferred one).
func TestVisionDispatcher_RespectsAllowList(t *testing.T) {
	var gotModel string
	d := &visionDispatcher{
		provider:      "anthropic",
		allowedModels: []string{"claude-sonnet-4-6"}, // not the preferred haiku
		maxTokens:     2048,
		forward: func(_ context.Context, _ []byte, model string) ([]byte, int, error) {
			gotModel = model
			return anthropicVisionResp("ok", 10, 5), 200, nil
		},
	}
	res, err := d.DispatchVision(context.Background(), visionReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "claude-sonnet-4-6" || gotModel != "claude-sonnet-4-6" {
		t.Errorf("allow-list not respected: res=%q forwarded=%q", res.Model, gotModel)
	}
}

// An allow-list that excludes every capable model → error → graceful passthrough.
func TestVisionDispatcher_AllowListExcludesAllCapable(t *testing.T) {
	d := &visionDispatcher{
		provider:      "anthropic",
		allowedModels: []string{"gpt-4o"}, // no Anthropic model allowed
		maxTokens:     4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			t.Fatal("forward must NOT be called when the allow-list excludes all capable models")
			return nil, 0, nil
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("expected an error when no allowed model is document-capable")
	}
}

// A transport/upstream error from forward surfaces as a dispatch error (graceful
// passthrough upstream).
func TestVisionDispatcher_ForwardError(t *testing.T) {
	d := &visionDispatcher{
		provider:  "anthropic",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			return nil, 0, errors.New("dial timeout")
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("a forward error must surface as a dispatch error")
	}
}

// A non-200 upstream status is an error (no usable OCR).
func TestVisionDispatcher_Non200(t *testing.T) {
	d := &visionDispatcher{
		provider:  "anthropic",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			return []byte(`{"error":"overloaded"}`), 529, nil
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("a non-200 upstream status must surface as a dispatch error")
	}
}

// A 200 with no text content → error, so visionFallback stays NeedsVision rather
// than substituting empty Markdown.
func TestVisionDispatcher_EmptyContent(t *testing.T) {
	d := &visionDispatcher{
		provider:  "anthropic",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			return []byte(`{"content":[],"usage":{"input_tokens":5,"output_tokens":0}}`), 200, nil
		},
	}
	if _, err := d.DispatchVision(context.Background(), visionReq()); err == nil {
		t.Fatal("an empty-content response must surface as a dispatch error")
	}
}

// A provider response whose usage block is present but ALL-ZERO, alongside
// recovered text, must NOT be recorded as a free OCR: it falls through to the
// conservative non-zero estimate (honest cost accounting — a present-but-zero
// usage is as untrustworthy as a missing one when text was clearly recovered).
func TestVisionDispatcher_ZeroReportedUsageEstimates(t *testing.T) {
	d := &visionDispatcher{
		provider:  "anthropic",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			return []byte(`{"content":[{"type":"text","text":"recovered text here"}],"usage":{"input_tokens":0,"output_tokens":0}}`), 200, nil
		},
	}
	res, err := d.DispatchVision(context.Background(), visionReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens <= 0 || res.OutputTokens <= 0 {
		t.Errorf("all-zero reported usage with recovered text must be ESTIMATED non-zero, never free; in=%d out=%d", res.InputTokens, res.OutputTokens)
	}
}

// extractAnthropicText concatenates only the text blocks of a native response.
func TestExtractAnthropicText(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"a"},{"type":"thinking","text":"x"},{"type":"text","text":"b"}]}`)
	if got := extractAnthropicText(body); got != "ab" {
		t.Errorf("extractAnthropicText = %q want \"ab\"", got)
	}
	if got := extractAnthropicText([]byte("not json")); got != "" {
		t.Errorf("malformed body must yield empty string; got %q", got)
	}
}

// When the provider reports no usage, the dispatcher estimates a non-zero cost
// rather than recording a free OCR (honest cost accounting).
func TestVisionDispatcher_EstimatesCostWhenUsageMissing(t *testing.T) {
	d := &visionDispatcher{
		provider:  "anthropic",
		maxTokens: 4096,
		forward: func(context.Context, []byte, string) ([]byte, int, error) {
			return []byte(`{"content":[{"type":"text","text":"recovered text here"}]}`), 200, nil
		},
	}
	res, err := d.DispatchVision(context.Background(), visionReq())
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens <= 0 || res.OutputTokens <= 0 {
		t.Errorf("missing usage must be ESTIMATED non-zero, never recorded as free; in=%d out=%d", res.InputTokens, res.OutputTokens)
	}
	if !strings.Contains(res.Markdown, "recovered text") {
		t.Errorf("markdown lost: %q", res.Markdown)
	}
}
