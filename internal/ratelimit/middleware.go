package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/metrics"
)

// RateLimitMiddleware fronts a Limiter on the request path. The wsID is
// read from X-Talyvor-Workspace (already authoritative — AuthMiddleware
// overwrites client-supplied values with the key owner's workspace). The
// keyID comes from the validated APIKey on the context if present.
//
// On rejection: 429 with Retry-After + X-RateLimit-Remaining headers and
// a structured JSON body. On acceptance: X-RateLimit-Remaining is set so
// clients see their budget.
func RateLimitMiddleware(l *Limiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wsID := r.Header.Get("X-Talyvor-Workspace")
			keyID := ""
			if k := auth.GetAPIKey(r.Context()); k != nil {
				keyID = k.ID
			}
			result := l.Check(r.Context(), wsID, keyID)
			if !result.Allowed {
				metrics.RateLimitRejected(wsID)
				w.Header().Set("Retry-After", strconv.Itoa(result.RetryAfterSecs))
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":               "rate limit exceeded",
					"limit_type":          result.LimitType,
					"retry_after_seconds": result.RetryAfterSecs,
				})
				return
			}
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(result.Remaining))
			next.ServeHTTP(w, r)
		})
	}
}
