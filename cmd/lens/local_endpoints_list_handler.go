package main

import (
	"net/http"

	"github.com/talyvor/lens/internal/localrouter"
)

// local_endpoints_list_handler.go — GET /v1/local/endpoints, and the one field it
// shipped that its own published contract never declared.
//
// ⚠ THE CONTRACT IS NOT A MATTER OF OPINION HERE: this binary serves it, at
// GET /openapi.json, from internal/api/openapi.go. The `LocalEndpoint` schema
// declares exactly ten properties —
//
//	id · url · provider · models · priority · max_concurrent · active · healthy ·
//	avg_latency_ms · error_rate
//
// — and the route shipped THREE the contract does not declare:
//
//	workspace_id   the tenant that owns the node          ← the leak
//	last_check_at  when it was last health-probed
//	active_count   how many requests are in flight on it
//
// ⚠ AND THE WIRE SHAPE IS NOT DERIVABLE FROM THE STRUCT TAGS, which is why the
// guard beside this file compares the RESPONSE BYTES and not the type:
// localrouter.LocalEndpoint has a custom MarshalJSON that folds the unexported
// activeCount in as `active_count`. A parity check written against struct tags
// would have reported two drifted fields and missed the third.
//
// The repair is one principle applied uniformly rather than a judgement per
// field: the response carries exactly what the published schema declares. If
// somebody wants last_check_at or active_count published, they add them to the
// schema and the guard then REQUIRES the response to carry them.
//
// This route is inside the `authed` group with no further gate, so that went to
// every authenticated caller — while POST and DELETE on this same path are both
// requireAdmin. The writes were gated and the read was not.
//
// ⚠ WHAT THIS DOES NOT DO, AND WHY — SAID PLAINLY RATHER THAN LEFT TO BE NOTICED.
// `url` STAYS. It is another tenant's endpoint and it is the more sensitive half,
// but the published schema DECLARES it, so removing it is a change to a
// documented contract rather than the removal of something that was never
// promised. Same for the missing gate: making this read requireAdmin — which is
// what its own POST and DELETE use — changes who may call a documented operation.
// Both are decisions, both are recorded in W6.15, and neither is a session's to
// take. What is fixed here is the part that needs no decision: a field the
// contract does not declare, naming somebody else.

// localEndpointLister is the slice of *localrouter.Router this route needs.
type localEndpointLister interface {
	List() []*localrouter.LocalEndpoint
}

// publicLocalEndpoint is the published LocalEndpoint schema, field for field.
// Keep it in lockstep with internal/api/openapi.go's components.schemas —
// local_endpoints_list_handler_test.go compares the two in both directions and
// fails on any drift, so they cannot separate silently again.
type publicLocalEndpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	Models        []string `json:"models"`
	Priority      int      `json:"priority"`
	MaxConcurrent int      `json:"max_concurrent"`
	Active        bool     `json:"active"`
	Healthy       bool     `json:"healthy"`
	AvgLatencyMs  int64    `json:"avg_latency_ms"`
	ErrorRate     float64  `json:"error_rate"`
}

func publicLocalEndpointView(eps []*localrouter.LocalEndpoint) []publicLocalEndpoint {
	out := make([]publicLocalEndpoint, 0, len(eps))
	for _, e := range eps {
		if e == nil {
			continue
		}
		out = append(out, publicLocalEndpoint{
			ID: e.ID, URL: e.URL, Provider: e.Provider,
			Models: append([]string{}, e.Models...), Priority: e.Priority,
			MaxConcurrent: e.MaxConcurrent, Active: e.Active, Healthy: e.Healthy,
			AvgLatencyMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate,
		})
	}
	return out
}

func newLocalEndpointsListHandler(l localEndpointLister) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONOK(w, http.StatusOK, publicLocalEndpointView(l.List()))
	}
}
