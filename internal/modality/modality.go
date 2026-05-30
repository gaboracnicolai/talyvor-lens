// Package modality detects the modalities (text / image / audio / document)
// present in a chat request and knows which models can serve them, so the
// proxy can route a multimodal request to a CAPABLE model rather than
// silently flattening an image into text at a text-only model.
//
// Two design rules:
//
//   - Detection is STRUCTURAL and cheap: it inspects the content-block
//     `type` fields only. It never decodes the base64 image/audio bytes —
//     the minimal structs it unmarshals into don't even have the data
//     fields, so the JSON decoder skips them.
//   - Capability is CONSERVATIVE: a model whose capabilities are unknown is
//     treated as text-only. We never assume a model can do vision.
package modality

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Per-modality input-token estimates (documented, rough). Exact image-token
// accounting needs the rendered image dimensions or the provider's returned
// usage; until that's wired, multimodal input cost is an ESTIMATE built from
// the text length plus these per-item figures, and the spend row is flagged
// cost_estimated.
const (
	ImageTokenEstimate = 1000
	AudioTokenEstimate = 1000
)

// ModalitySet is what a request contains. TextChars is the total length of
// the text blocks only (image/audio bytes excluded), so callers can build a
// sane token estimate instead of counting the base64 blob.
type ModalitySet struct {
	HasImage    bool `json:"has_image"`
	HasAudio    bool `json:"has_audio"`
	HasDocument bool `json:"has_document"`
	ImageCount  int  `json:"image_count"`
	AudioCount  int  `json:"audio_count"`
	TextChars   int  `json:"text_chars"`
}

// Multimodal reports whether any non-text modality is present.
func (m ModalitySet) Multimodal() bool { return m.HasImage || m.HasAudio || m.HasDocument }

// Label is the canonical, low-cardinality modality string for metrics +
// the spend record: "text", or the comma-joined non-text modalities.
func (m ModalitySet) Label() string {
	var parts []string
	if m.HasImage {
		parts = append(parts, "image")
	}
	if m.HasAudio {
		parts = append(parts, "audio")
	}
	if m.HasDocument {
		parts = append(parts, "document")
	}
	if len(parts) == 0 {
		return "text"
	}
	return strings.Join(parts, ",")
}

// EstimateInputTokens returns a modality-aware input-token estimate: text
// chars / 4 plus the per-item estimates. Used for multimodal requests in
// place of len(rawBody)/4, which would count the base64 blob.
func (m ModalitySet) EstimateInputTokens() int {
	return m.TextChars/4 + m.ImageCount*ImageTokenEstimate + m.AudioCount*AudioTokenEstimate
}

// ─── detection ───

// detectShape captures only what detection needs — message content as raw
// JSON, decoded block-by-block below. No data/url fields, so base64 payloads
// are never decoded.
type detectShape struct {
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// contentBlock is the union of the OpenAI + Anthropic block shapes, with ONLY
// the structural fields (type, text, source.type/media_type). The base64
// data fields (image_url.url, source.data, input_audio.data) are deliberately
// absent so they're skipped, never decoded.
type contentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
	} `json:"source"`
}

// Detect inspects a chat-completions body and reports the modalities present.
// Recognises the OpenAI content-array shape (type "image_url" / "input_audio")
// and the Anthropic block shape (type "image" / "document" with a source).
// A parse failure yields an all-false set (treated as text) — detection must
// never break a request.
func Detect(body []byte) ModalitySet {
	var ms ModalitySet
	var req detectShape
	if err := json.Unmarshal(body, &req); err != nil {
		return ms
	}
	for _, m := range req.Messages {
		c := bytes.TrimSpace(m.Content)
		if len(c) == 0 {
			continue
		}
		// String content → plain text.
		if c[0] == '"' {
			var s string
			if json.Unmarshal(c, &s) == nil {
				ms.TextChars += len(s)
			}
			continue
		}
		// Array content → inspect each block's type (no data decode).
		if c[0] == '[' {
			var blocks []contentBlock
			if json.Unmarshal(c, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					ms.TextChars += len(b.Text)
				case "image_url", "image":
					ms.HasImage = true
					ms.ImageCount++
				case "input_audio", "audio":
					ms.HasAudio = true
					ms.AudioCount++
				case "document", "file":
					ms.HasDocument = true
				}
			}
		}
	}
	return ms
}

// ─── capability registry ───

// Capabilities describes the non-text modalities a model can serve.
type Capabilities struct {
	Vision   bool `json:"vision"`
	Audio    bool `json:"audio"`
	Document bool `json:"document"`
}

// capabilities is the seed registry. A model NOT listed here is text-only by
// default (the conservative case). Keep it data-driven — add a row to extend.
var capabilities = map[string]Capabilities{
	// OpenAI GPT-4o / 4.1 / 5.4 families — vision.
	"gpt-4o":       {Vision: true},
	"gpt-4o-mini":  {Vision: true},
	"gpt-4.1":      {Vision: true},
	"gpt-4.1-mini": {Vision: true},
	"gpt-4.1-nano": {Vision: true},
	"gpt-5.4":      {Vision: true},
	"gpt-5.4-mini": {Vision: true},
	// Anthropic Claude 4 family — vision + document (PDF).
	"claude-opus-4-5":   {Vision: true, Document: true},
	"claude-opus-4-6":   {Vision: true, Document: true},
	"claude-sonnet-4-5": {Vision: true, Document: true},
	"claude-sonnet-4-6": {Vision: true, Document: true},
	"claude-haiku-4-5":  {Vision: true, Document: true},
	"claude-haiku-4-6":  {Vision: true, Document: true},
	// Google Gemini — vision + audio + document.
	"gemini-1.5-pro":   {Vision: true, Audio: true, Document: true},
	"gemini-1.5-flash": {Vision: true, Audio: true, Document: true},
	"gemini-2.0-flash": {Vision: true, Audio: true, Document: true},
	"gemini-2.5-flash": {Vision: true, Audio: true, Document: true},
	"gemini-2.5-pro":   {Vision: true, Audio: true, Document: true},
	// AWS Bedrock Claude — vision + document.
	"anthropic.claude-opus-4-6-20251101-v1:0":   {Vision: true, Document: true},
	"anthropic.claude-sonnet-4-6-20251101-v1:0": {Vision: true, Document: true},
	"anthropic.claude-haiku-4-6-20251103-v1:0":  {Vision: true, Document: true},
	// Mistral (listed models) + Groq open models are text-only — omitted, so
	// they fall through to the conservative text-only default.
}

// providerPreference lists each provider's multimodal-capable models cheapest
// first, used to pick a redirect target.
var providerPreference = map[string][]string{
	"openai":    {"gpt-4o-mini", "gpt-4.1-mini", "gpt-4.1", "gpt-4o", "gpt-5.4-mini", "gpt-5.4"},
	"anthropic": {"claude-haiku-4-6", "claude-haiku-4-5", "claude-sonnet-4-6", "claude-sonnet-4-5", "claude-opus-4-6", "claude-opus-4-5"},
	"google":    {"gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-flash", "gemini-2.5-pro", "gemini-1.5-pro"},
	"bedrock":   {"anthropic.claude-haiku-4-6-20251103-v1:0", "anthropic.claude-sonnet-4-6-20251101-v1:0", "anthropic.claude-opus-4-6-20251101-v1:0"},
}

// Get returns a model's capabilities (zero value = text-only for unknowns).
func Get(model string) Capabilities { return capabilities[model] }

// Supports reports whether a model can serve every modality the request
// needs. A text-only request (all-false set) is supported by every model.
func Supports(model string, need ModalitySet) bool {
	c := capabilities[model]
	if need.HasImage && !c.Vision {
		return false
	}
	if need.HasAudio && !c.Audio {
		return false
	}
	if need.HasDocument && !c.Document {
		return false
	}
	return true
}

// CapableModel returns the cheapest model of `provider` that serves `need`
// and is in the workspace's allowed set (empty allowed = all). Used to
// redirect an auto-route request whose chosen model can't handle the
// modality. Returns ok=false when no capable allowed model exists.
func CapableModel(provider string, need ModalitySet, allowed []string) (string, bool) {
	for _, m := range providerPreference[provider] {
		if len(allowed) > 0 && !contains(allowed, m) {
			continue
		}
		if Supports(m, need) {
			return m, true
		}
	}
	return "", false
}

// CapabilityMap returns a copy of the registry for the introspection API.
func CapabilityMap() map[string]Capabilities {
	out := make(map[string]Capabilities, len(capabilities))
	for k, v := range capabilities {
		out[k] = v
	}
	return out
}

// KnownModels returns the registry's model names, sorted (deterministic API).
func KnownModels() []string {
	out := make([]string, 0, len(capabilities))
	for k := range capabilities {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hs []string, needle string) bool {
	for _, h := range hs {
		if h == needle {
			return true
		}
	}
	return false
}
