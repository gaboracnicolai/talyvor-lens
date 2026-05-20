package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lens_requests_total",
			Help: "Total number of proxied requests, partitioned by provider and outcome.",
		},
		[]string{"provider", "outcome"},
	)

	CacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lens_cache_hits_total",
			Help: "Total number of cache hits, partitioned by cache layer.",
		},
		[]string{"layer"},
	)

	TokensSavedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "lens_tokens_saved_total",
			Help: "Total tokens saved, partitioned by provider and strategy.",
		},
		[]string{"provider", "strategy"},
	)
)

func init() {
	prometheus.MustRegister(RequestsTotal, CacheHitsTotal, TokensSavedTotal)
}

func Handler() http.Handler {
	return promhttp.Handler()
}
