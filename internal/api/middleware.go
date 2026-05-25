package api

// middleware.go — cross-cutting HTTP middleware that the chi
// router installs once. Each middleware here is small, opt-in
// at registration time, and has no dependency on the existing
// Server struct so cmd/lens/main.go can wire them up wherever
// it likes.
//
// Pieces:
//   - RequestIDMiddleware   — X-Request-ID propagation
//   - APIVersionMiddleware  — X-API-Version on every response
//   - GzipMiddleware        — opt-in gzip for large JSON bodies
//   - RateLimitHeaders      — emits X-RateLimit-* on responses
//   - APIError / WriteError — standardised error envelope

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ─── constants ───────────────────────────────────

// APIVersion is the version string emitted on every response.
// Bumped together with breaking API changes.
const APIVersion = "1.0.0"

// GzipMinBytes is the threshold below which gzip would just
// add overhead. 1KB matches the spec.
const GzipMinBytes = 1024

// Standard error codes — SCREAMING_SNAKE_CASE per spec.
const (
	ErrCodeUnauthorized        = "UNAUTHORIZED"
	ErrCodeForbidden           = "FORBIDDEN"
	ErrCodeNotFound            = "NOT_FOUND"
	ErrCodeRateLimited         = "RATE_LIMITED"
	ErrCodeSpendCapExceeded    = "SPEND_CAP_EXCEEDED"
	ErrCodeInvalidRequest      = "INVALID_REQUEST"
	ErrCodeInternalError       = "INTERNAL_ERROR"
	ErrCodeModelNotAllowed     = "MODEL_NOT_ALLOWED"
	ErrCodeProviderUnavailable = "PROVIDER_UNAVAILABLE"
)

// ─── request ID middleware ───────────────────────

// requestIDKey is the context key under which the chosen
// request ID is stored. Loggers can pluck it back out via
// RequestIDFromContext.
type ctxKey string

const requestIDKey ctxKey = "talyvor.request_id"

// RequestIDMiddleware sets X-Request-ID. If the client already
// supplied one we trust it (so distributed tracing works);
// otherwise we mint a fresh UUID.
type RequestIDMiddleware struct{}

func (m *RequestIDMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the ID stamped onto this request
// by RequestIDMiddleware. Empty string when no middleware ran
// (handy for unit-testing helpers in isolation).
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// ─── API-version middleware ──────────────────────

// APIVersionMiddleware tags every response with the current
// X-API-Version header. Clients can use it to detect upgrades.
func APIVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-API-Version", APIVersion)
		next.ServeHTTP(w, r)
	})
}

// ─── gzip middleware ─────────────────────────────

// gzipResponseWriter buffers the response so we can decide
// post-hoc whether to compress. Small bodies fall through
// untouched, large bodies get gzipped before flush.
type gzipResponseWriter struct {
	http.ResponseWriter
	buf        bytes.Buffer
	status     int
	wroteStatus bool
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	g.status = status
	g.wroteStatus = true
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.buf.Write(b)
}

// flush is called by the middleware once the handler returns —
// it decides between passthrough and gzip based on size + the
// Accept-Encoding negotiation captured at entry.
func (g *gzipResponseWriter) flush(allowGzip bool) {
	if !g.wroteStatus {
		g.status = http.StatusOK
	}
	body := g.buf.Bytes()
	if allowGzip && len(body) >= GzipMinBytes {
		g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
		g.ResponseWriter.Header().Del("Content-Length")
		g.ResponseWriter.WriteHeader(g.status)
		gw := gzip.NewWriter(g.ResponseWriter)
		_, _ = gw.Write(body)
		_ = gw.Close()
		return
	}
	g.ResponseWriter.WriteHeader(g.status)
	_, _ = g.ResponseWriter.Write(body)
}

// GzipMiddleware compresses JSON-ish responses larger than
// 1KB when the client says it accepts gzip. We deliberately
// skip compression on websocket / SSE / already-encoded
// streams — those handlers write to w directly and the
// buffered wrapper short-circuits via the Hijacker path.
func GzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
		// SSE / event-stream MUST stream — never buffer.
		if strings.HasPrefix(r.URL.Path, "/v1/proxy/") ||
			strings.HasPrefix(r.URL.Path, "/oai/") ||
			strings.HasPrefix(r.URL.Path, "/anthropic/") ||
			strings.HasPrefix(r.URL.Path, "/mcp") {
			next.ServeHTTP(w, r)
			return
		}
		gz := &gzipResponseWriter{ResponseWriter: w}
		next.ServeHTTP(gz, r)
		gz.flush(acceptsGzip)
	})
}

// ─── rate-limit-headers middleware ──────────────

// RateLimitInfo is what the existing rate limiter stashes onto
// the request context. We don't ship a duplicate limiter here —
// just a header writer that picks the values up if they're
// present and emits the standard X-RateLimit-* triple.
type RateLimitInfo struct {
	Limit     int   // requests/window
	Remaining int   // requests left in current window
	Reset     int64 // unix seconds when the window resets
}

// rateLimitCtxKey lives in the same scope as requestIDKey so
// the limiter and the header middleware can hand state across
// without exporting a separate package.
const rateLimitCtxKey ctxKey = "talyvor.rate_limit_info"

// WithRateLimit stamps RateLimitInfo onto the context. The
// existing limiter can call this from its own middleware so
// we don't have to refactor ratelimit/.
func WithRateLimit(ctx context.Context, info RateLimitInfo) context.Context {
	return context.WithValue(ctx, rateLimitCtxKey, info)
}

// RateLimitHeadersMiddleware reads the info that an upstream
// limiter has stamped and emits the standard triple. Silent
// no-op when no limiter ran for this route.
func RateLimitHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &headerCaptureWriter{ResponseWriter: w, req: r}
		next.ServeHTTP(rw, r)
	})
}

// headerCaptureWriter delays the WriteHeader call until we've
// had a chance to look up the rate-limit info that the handler
// (or a deeper middleware) might have stashed. Once the handler
// returns we copy the values onto the response.
type headerCaptureWriter struct {
	http.ResponseWriter
	req     *http.Request
	written bool
}

func (h *headerCaptureWriter) WriteHeader(status int) {
	if !h.written {
		if info, ok := h.req.Context().Value(rateLimitCtxKey).(RateLimitInfo); ok {
			if info.Limit > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Limit", itoa(info.Limit))
			}
			if info.Limit > 0 || info.Remaining > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Remaining", itoa(info.Remaining))
			}
			if info.Reset > 0 {
				h.ResponseWriter.Header().Set("X-RateLimit-Reset", i64toa(info.Reset))
			}
		}
		h.written = true
	}
	h.ResponseWriter.WriteHeader(status)
}

func (h *headerCaptureWriter) Write(b []byte) (int, error) {
	if !h.written {
		h.WriteHeader(http.StatusOK)
	}
	return h.ResponseWriter.Write(b)
}

// itoa / i64toa avoid pulling fmt for trivial numeric formats.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func i64toa(n int64) string { return itoa(int(n)) }

// ─── error envelope ──────────────────────────────

// APIError is the standardised error shape every handler should
// emit via WriteError / WriteErrorDetails.
type APIError struct {
	Code      string      `json:"code"`
	Message   string      `json:"message"`
	Details   interface{} `json:"details,omitempty"`
	RequestID string      `json:"request_id,omitempty"`
}

// WriteError writes the canonical error envelope with no details.
func WriteError(w http.ResponseWriter, code, message string, status int, requestID string) {
	WriteErrorDetails(w, code, message, nil, status, requestID)
}

// WriteErrorDetails is the full-fat version when there's
// structured context worth surfacing (validation errors etc.).
func WriteErrorDetails(w http.ResponseWriter, code, message string, details interface{}, status int, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{
		Code:      code,
		Message:   message,
		Details:   details,
		RequestID: requestID,
	})
}

// Drain is the small helper used by middleware tests to read
// a response body without leaking the reader.
func Drain(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}
