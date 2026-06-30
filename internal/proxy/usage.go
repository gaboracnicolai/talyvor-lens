package proxy

import "github.com/talyvor/lens/internal/inference"

// Usage is a provider's reported token accounting for one response. The type, the per-provider extractors,
// and the ExtractUsage method all live in internal/inference (the method moved with the ProviderConfig
// type in PR-3c); this alias keeps every proxy spend site referencing `Usage` unedited.
type Usage = inference.Usage
