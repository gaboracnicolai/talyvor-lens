// Package compat provides transparent compatibility shims for clients
// integrated with other LLM gateways. The Helicone middleware is the
// first of these — it accepts requests in Helicone's exact wire format
// (URL prefix + header names) and translates them into the Talyvor
// Lens shape before downstream middleware + handlers run.
//
// The translation is intentionally one-way: Lens never speaks Helicone
// back to the client. Migrating teams just change their base URL and
// every existing Helicone-Auth / Helicone-Property-* header keeps
// working.
package compat

import (
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/auth"
)

// HeliconeCompat owns the Helicone → Lens translation. The keyStore is
// reserved for future helicone_key_map lookups (when a tenant's
// existing Helicone keys need to be mapped to Lens-issued credentials);
// today the middleware passes the Helicone-Auth value through verbatim
// and lets the downstream auth middleware validate it.
type HeliconeCompat struct {
	keyStore *auth.KeyStore
}

func NewHeliconeCompat(keyStore *auth.KeyStore) *HeliconeCompat {
	return &HeliconeCompat{keyStore: keyStore}
}

// Middleware returns an http middleware that translates Helicone wire
// format requests into the Talyvor Lens shape. Safe to install
// globally — requests without Helicone headers pass through unchanged.
func (h *HeliconeCompat) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.translateHeaders(r)
			h.rewritePath(r)
			next.ServeHTTP(w, r)
		})
	}
}

// translateHeaders maps Helicone's header set onto Lens's. Headers are
// stripped after translation so the LLM upstream never sees them.
func (h *HeliconeCompat) translateHeaders(r *http.Request) {
	if hAuth := r.Header.Get("Helicone-Auth"); hAuth != "" {
		// Helicone-Auth carries the Talyvor key when the migration is
		// done; some migrating clients still send their old OpenAI key
		// in Authorization. Overwrite Authorization with the Helicone
		// value so the downstream auth middleware sees the Lens key.
		r.Header.Set("Authorization", hAuth)
		r.Header.Del("Helicone-Auth")
	}

	if uid := r.Header.Get("Helicone-User-Id"); uid != "" {
		// Only set X-Talyvor-Session when the caller hasn't already.
		// A migrating client sending both gets the explicit one honoured.
		if r.Header.Get("X-Talyvor-Session") == "" {
			r.Header.Set("X-Talyvor-Session", uid)
		}
		r.Header.Del("Helicone-User-Id")
	}

	// Helicone-Property-<Name>: <value> → X-Talyvor-Feature (first one).
	// Helicone supports arbitrary property names for grouping; Lens
	// uses a single "feature" attribution bucket per request, so we
	// take the first property as the feature and strip the rest.
	const propertyPrefix = "helicone-property-"
	var propertyKeys []string
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), propertyPrefix) {
			propertyKeys = append(propertyKeys, name)
		}
	}
	for i, name := range propertyKeys {
		if i == 0 && r.Header.Get("X-Talyvor-Feature") == "" {
			r.Header.Set("X-Talyvor-Feature", r.Header.Get(name))
		}
		r.Header.Del(name)
	}

	// Cache + retry are always-on in Lens — drop the toggle headers so
	// they don't confuse the upstream provider when forwarded.
	r.Header.Del("Helicone-Cache-Enabled")
	r.Header.Del("Helicone-Retry-Enabled")
}

// rewritePath maps Helicone's URL prefixes to Lens's proxy paths. Used
// alongside direct chi routes — the path rewrite normalises r.URL.Path
// for any downstream handler that cares (logging, span attributes).
func (h *HeliconeCompat) rewritePath(r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/oai/"):
		r.URL.Path = "/v1/proxy/openai/" + strings.TrimPrefix(r.URL.Path, "/oai/")
	case strings.HasPrefix(r.URL.Path, "/anthropic/"):
		r.URL.Path = "/v1/proxy/anthropic/" + strings.TrimPrefix(r.URL.Path, "/anthropic/")
	}
}
