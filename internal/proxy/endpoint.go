package proxy

import (
	"net/http"
	"strings"
)

// endpoint.go — WHICH upstream endpoint an inbound proxy request is for.
//
// Every provider's upstreamURLFn ignores the request path and returns a FIXED chat-completions
// URL (openai/anthropic/mistral/groq/vllm; google and bedrock derive theirs from the model). That
// was invisible while chat was the only endpoint anyone called. It stopped being invisible when
// talyvor-code v0.1.0 shipped: its semantic index POSTs /v1/proxy/openai/v1/embeddings, Lens
// forwarded the body to /v1/chat/completions, and OpenAI answered
// 400 "you must provide a messages parameter". The flagship feature of a released CLI could not
// work, and the cause was a discarded path.
//
// ⚠ AN ALLOWLIST, NOT PASSTHROUGH. Forwarding whatever suffix arrived would turn Lens into an
// open proxy to the provider with the operator's key — /v1/files, /v1/fine_tuning, /v1/batches —
// none of which Lens meters, prices, or has a spend row for. An endpoint Lens cannot bill is an
// endpoint it must not forward. Adding one here is deliberate and comes with its pricing.
type upstreamEndpoint uint8

const (
	// endpointChat is the default and everything that existed before: the OpenAI-shaped
	// chat-completions body every provider config already targets.
	endpointChat upstreamEndpoint = iota
	// endpointEmbeddings is POST {base}/v1/embeddings — text in, vectors out. No messages, no
	// completion tokens, and (see below) no business in the prompt cache.
	endpointEmbeddings
)

func (e upstreamEndpoint) String() string {
	if e == endpointEmbeddings {
		return "embeddings"
	}
	return "chat"
}

// classifyEndpoint reads the inbound path suffix. Anything not on the allowlist is endpointChat —
// which preserves today's behaviour exactly for every route that already worked, so this can only
// change the requests that are currently broken.
func classifyEndpoint(r *http.Request) upstreamEndpoint {
	if r == nil || r.URL == nil {
		return endpointChat
	}
	p := strings.TrimSuffix(r.URL.Path, "/")
	if strings.HasSuffix(p, "/v1/embeddings") || strings.HasSuffix(p, "/embeddings") {
		return endpointEmbeddings
	}
	return endpointChat
}

// embeddingsURLFor maps a provider's configured chat URL onto its embeddings URL.
//
// It rewrites the SUFFIX of the URL the provider config already produced rather than building a
// new base, so an operator's override (a proxy, a compatible gateway, a self-hosted vLLM) is
// honoured for embeddings exactly as it is for chat. ok=false means this provider has no
// embeddings endpoint Lens knows how to reach — the caller then refuses rather than silently
// forwarding an embeddings body to a chat URL, which is the bug this file exists to remove.
func embeddingsURLFor(provider, chatURL string) (string, bool) {
	switch provider {
	case "openai", "vllm":
		// Both expose the OpenAI shape: .../v1/chat/completions → .../v1/embeddings
		if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
			return chatURL[:i] + "/embeddings", true
		}
	case "groq":
		// .../openai/v1/chat/completions → .../openai/v1/embeddings
		if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
			return chatURL[:i] + "/embeddings", true
		}
	case "mistral":
		if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
			return chatURL[:i] + "/embeddings", true
		}
	}
	// anthropic has no embeddings API at all; google and bedrock use a different request shape
	// entirely (their URLs are model-derived, not path-derived), so an OpenAI-shaped embeddings
	// body could not be served by either. Refusing is the honest answer for all three.
	return "", false
}

// cacheable reports whether this endpoint's responses may enter the prompt caches.
//
// ⚠ EMBEDDINGS MUST NOT, AND THIS IS THE SHARPEST EDGE IN THE CHANGE. The caches key on the
// prompt that extractPrompt derives, and extractPrompt reads `messages` — an embeddings body has
// `input` instead, so EVERY embeddings request extracts the empty string. Measured:
//
//	embed("file A") → cache key "ws-1:"
//	embed("file B") → cache key "ws-1:"   ← same key
//
// So opening the route without this guard would replace a loud 400 with a silent wrong answer:
// the second chunk of an index build would be served the FIRST chunk's vector, and every chunk
// after it likewise. The index would be quietly garbage and nothing would error. A 400 is a bad
// day; a silently corrupted semantic index is a bad quarter.
//
// Keying embeddings correctly (on the input) is possible and would even be useful — identical
// chunks recur constantly in a codebase. It is deliberately NOT done here: it would put source
// code into a cache that can be POOLED CROSS-TENANT when a workspace opts in, and whether one
// company's code embeddings may be served to another is a product decision, not a bug fix.
// Reported, not decided. Until then embeddings are simply not cached.
func (e upstreamEndpoint) cacheable() bool { return e == endpointChat }
