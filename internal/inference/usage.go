package inference

import "encoding/json"

// Usage is a provider's REPORTED token accounting for one response. Moved from internal/proxy (PR-3b A′);
// proxy keeps a `type Usage = inference.Usage` alias so its spend sites stay unedited.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ExtractUsage reads the provider's reported usage out of an upstream response body (ok=false when no
// usage block is present — the caller then estimates). It dispatches on the provider NAME (a single
// source of truth) rather than a per-config closure: by the spend site openai/mistral/groq/vllm pass
// through native OpenAI shape and google+bedrock are reverse-translated to OpenAI shape, so all read
// usage.prompt_tokens/completion_tokens; anthropic alone stays native (usage.input_tokens/output_tokens).
func ExtractUsage(provider string, body []byte) (Usage, bool) {
	if provider == "anthropic" {
		return extractAnthropicUsage(body)
	}
	return extractOpenAIUsage(body)
}

// extractOpenAIUsage reads the OpenAI-shape usage object. The pointer makes "field absent" distinguishable
// from "present but zero" so a missing block reports ok=false rather than billing a silent zero.
func extractOpenAIUsage(body []byte) (Usage, bool) {
	var r struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return Usage{}, false
	}
	return Usage{InputTokens: r.Usage.PromptTokens, OutputTokens: r.Usage.CompletionTokens}, true
}

// extractAnthropicUsage reads the Anthropic-native usage object.
func extractAnthropicUsage(body []byte) (Usage, bool) {
	var r struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Usage == nil {
		return Usage{}, false
	}
	return Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}, true
}
