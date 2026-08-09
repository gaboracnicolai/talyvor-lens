package auth

import "net/http"

// CredentialHeaders is the complete set of inbound header names Lens will accept a Talyvor
// credential in — the same three locations extractCredential (manager.go) and extractKey
// (middleware.go) read, spelled the same way.
//
// ⚠ THIS IS A SECOND COPY OF A FACT THE EXTRACTORS ALREADY STATE IN CODE, and two copies of one
// fact with nothing between them is how the other one goes stale. The thing between them is
// TestCredentialHeaderSet_MatchesAuthSource in internal/proxy: it PARSES both extractors, collects
// every header name they read, and fails in BOTH directions — a fourth credential location added to
// auth fails until it is listed here, and an entry left here after its extractor branch is deleted
// fails as a stale pin.
//
// The extractors are deliberately NOT rewritten to loop over this slice. They carry per-location
// semantics (the "Bearer " prefix on Authorization, the priority order between the three), and
// rewriting the credential path to remove a duplication is a larger change, on a more dangerous
// path, than the leak this slice exists to close.
var CredentialHeaders = []string{"Authorization", "X-Talyvor-Key", "X-API-Key"}

// StripCredentialHeaders returns a COPY of h with every credential location removed.
//
// WHY A COPY: the inbound *http.Request's own header map is read again after the upstream call (the
// request context, logging, attribution), so deleting in place would change what the rest of the
// serve path sees. The cost is one header-map clone per upstream attempt.
//
// A nil header stays nil — http.Header.Clone() already answers that way, and a caller with no
// inbound request (the background scorer passes nil) must keep getting nil rather than an empty map
// it would then iterate over.
func StripCredentialHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return nil
	}
	for _, name := range CredentialHeaders {
		out.Del(name)
	}
	return out
}
