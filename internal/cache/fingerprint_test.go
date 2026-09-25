package cache

// testFP is the request fingerprint (B15.1) the cache-level tests store and look up under. Those
// tests exercise similarity, entities and provenance, so any one fingerprint used on both sides
// leaves them measuring what they measured before; the fingerprint itself is proven through the real
// handler in internal/proxy/request_fingerprint_realpg_test.go.
const testFP = "test-fp"
