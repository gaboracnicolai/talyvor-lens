package proxy

import "github.com/talyvor/lens/internal/inference"

// Usage is a provider's reported token accounting for one response. The type + the per-provider
// extractors moved to internal/inference (PR-3b A′); this alias keeps every proxy spend site referencing
// `Usage` unedited.
type Usage = inference.Usage

// ExtractUsage stays a providerConfig method (so the Provider interface + vision_dispatch's inline
// providerConfig{name:provider}.ExtractUsage call are unchanged); it now delegates to the moved
// name-dispatched free function in internal/inference.
func (c providerConfig) ExtractUsage(body []byte) (Usage, bool) {
	return inference.ExtractUsage(c.name, body)
}
