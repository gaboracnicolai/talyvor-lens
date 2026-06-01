package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// apiKeyContextKey is the request-context slot for the validated APIKey.
// Use GetAPIKey to extract it in downstream handlers.
type apiKeyContextKey struct{}

// AuthMiddleware returns a chi-compatible middleware that requires every
// request to carry a valid credential.
//
// Validation order:
//  1. DB keystore (hot path for normal workspace/team keys).
//  2. Manager fallback — handles the global admin key (LENS_API_KEY) and
//     JWT bearer tokens, which are never in the DB and were silently
//     blocked before this fix.
//
// When Manager validates the credential it also stamps an AuthContext onto
// the request context (via authContextCtxKey), so downstream handlers can
// call GetAuthContext() without a second Authenticate() round-trip.
//
// The validated APIKey is attached to the request context so the rate-limiter
// (and any other GetAPIKey consumer) always sees a non-nil value.
// Workspace/team headers are overwritten with authoritative values from the
// resolved identity so handlers cannot be spoofed by client-supplied headers.
func AuthMiddleware(ks *KeyStore, m *Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractKey(r)
			if raw == "" {
				writeAuthError(w, http.StatusUnauthorized, "API key required")
				return
			}

			// ── Fast path: DB keystore ────────────────────────────────────
			// Handles normal workspace/team keys (tlv_ prefix, stored in
			// api_keys). Unchanged from the original implementation.
			result := ks.Validate(r.Context(), raw)
			if result.Valid {
				r.Header.Set("X-Talyvor-Workspace", result.APIKey.WorkspaceID)
				if result.APIKey.Team != "" {
					r.Header.Set("X-Talyvor-Team", result.APIKey.Team)
				}
				ctx := context.WithValue(r.Context(), apiKeyContextKey{}, result.APIKey)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// ── Manager fallback: global key + JWT ────────────────────────
			// The global admin key (LENS_API_KEY) and JWT bearer tokens are
			// never persisted to api_keys, so the DB lookup above always
			// misses for them. Delegate to Manager.Authenticate which knows
			// all four credential shapes.
			if m != nil {
				actx, err := m.Authenticate(r)
				if err == nil {
					// Synthesise a minimal APIKey so the rate-limiter and any
					// other GetAPIKey consumer gets a non-nil value.
					synthetic := &APIKey{
						ID:          "global",
						WorkspaceID: actx.WorkspaceID,
						Name:        actx.AuthMethod,
						Active:      true,
						CreatedAt:   time.Now().UTC(),
					}
					r.Header.Set("X-Talyvor-Workspace", actx.WorkspaceID)
					// Stamp both context slots so downstream code can use
					// either GetAPIKey or GetAuthContext without a second
					// Authenticate call.
					ctx := context.WithValue(r.Context(), apiKeyContextKey{}, synthetic)
					ctx = context.WithValue(ctx, authContextCtxKey{}, actx)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			writeAuthError(w, http.StatusUnauthorized, result.Reason)
		})
	}
}

// GetAPIKey extracts the validated APIKey from the request context.
// Returns nil if the request bypassed the middleware.
func GetAPIKey(ctx context.Context) *APIKey {
	if v, ok := ctx.Value(apiKeyContextKey{}).(*APIKey); ok {
		return v
	}
	return nil
}

// extractKey looks at Authorization: Bearer first, then X-Talyvor-Key.
func extractKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(h, "Bearer ") {
			return strings.TrimPrefix(h, "Bearer ")
		}
	}
	return r.Header.Get("X-Talyvor-Key")
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
