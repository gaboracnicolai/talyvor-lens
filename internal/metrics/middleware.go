package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// HTTPMiddleware records request count, latency, and in-flight gauge for every
// request. The route label is chi's matched route PATTERN (e.g.
// /v1/issues/{id}), never the raw path, so cardinality stays bounded. Unmatched
// routes bucket as "other".
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		InflightRequests.Inc()
		defer InflightRequests.Dec()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "other"
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // handler wrote body without explicit WriteHeader
		}
		HTTPRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(status)).Inc()
		HTTPRequestDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}
