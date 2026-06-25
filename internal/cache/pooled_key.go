package cache

// PoolKeyMarker namespaces the SHARED (cross-tenant) prompt-cache keyspace (exact + semantic) so
// it is PROVABLY disjoint from the workspace-private keyspace. A private key is hashed from
// "wsID:prompt" where wsID comes from the X-Talyvor-Workspace header (which cannot contain NUL);
// the marker's NUL bytes can never begin a private key's pre-image, so a tenant cannot craft a raw
// prompt that collides with a victim's private key. Single source of truth: the proxy serve path
// AND the L·seed warm-start tool key pooled entries via PooledPromptKey, so a seed lands exactly
// where the serve path reads (write-key == read-key).
const PoolKeyMarker = "\x00pool\x00"

// PooledPromptKey is the key material for the shared pool: the raw prompt under the NUL marker.
// Used as the `prompt` argument to ExactCache.SetWithOwner / SemanticCache.SetPooled for any
// cross-tenant entry.
func PooledPromptKey(prompt string) string { return PoolKeyMarker + prompt }
