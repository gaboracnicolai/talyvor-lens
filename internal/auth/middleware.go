package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// apiKeyContextKey is the request-context slot for the validated APIKey.
// Use GetAPIKey to extract it in downstream handlers.
type apiKeyContextKey struct{}

// AuthMiddleware returns a chi-compatible middleware that requires every
// request to carry a valid API key. The validated APIKey is attached to
// the request context, and the X-Talyvor-Workspace / X-Talyvor-Team
// headers are overwritten with the key's owner data so handlers can't be
// spoofed by client-supplied headers.
func AuthMiddleware(ks *KeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractKey(r)
			if raw == "" {
				writeAuthError(w, http.StatusUnauthorized, "API key required")
				return
			}
			result := ks.Validate(r.Context(), raw)
			if !result.Valid {
				writeAuthError(w, http.StatusUnauthorized, result.Reason)
				return
			}

			// Stamp the authoritative workspace/team headers from the key,
			// overwriting anything the client tried to set.
			r.Header.Set("X-Talyvor-Workspace", result.APIKey.WorkspaceID)
			if result.APIKey.Team != "" {
				r.Header.Set("X-Talyvor-Team", result.APIKey.Team)
			}

			ctx := context.WithValue(r.Context(), apiKeyContextKey{}, result.APIKey)
			next.ServeHTTP(w, r.WithContext(ctx))
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
