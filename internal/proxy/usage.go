package proxy

import "encoding/json"

// Usage is a provider's REPORTED token accounting for one response. When a
// provider surfaces it, spend is billed on these exact counts instead of the
// len/4 estimate — and for multimodal the reported input count already folds
// in the image cost, so the flat ImageTokenEstimate is only a fallback.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ExtractUsage reads the provider's reported usage out of an upstream
// response body, returning ok=false when no usage block is present (the
// caller then falls back to estimation — a usage-parse miss must never fail
// a request). It dispatches on the provider NAME rather than a per-config
// closure on purpose: providerConfigs are built at several sites (the inline
// Handle* literals and configForProvider), and a name switch is a single
// source of truth that can't be forgotten at one of them.
//
// By the time a response reaches the spend site it is already normalized:
// openai/mistral/groq/vllm pass through native OpenAI shape, and
// google+bedrock are reverse-translated to OpenAI shape — so all of those
// read usage.prompt_tokens/completion_tokens. Anthropic alone has no
// translateResponse, so its body stays native (usage.input_tokens/
// output_tokens).
func (c providerConfig) ExtractUsage(body []byte) (Usage, bool) {
	if c.name == "anthropic" {
		return extractAnthropicUsage(body)
	}
	return extractOpenAIUsage(body)
}

// extractOpenAIUsage reads the OpenAI-shape usage object. The pointer makes
// "field absent" distinguishable from "present but zero" so a missing block
// reports ok=false rather than billing a silent zero.
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
