package ha

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Dep is one readiness dependency check. The Name appears in the /readyz body;
// Check returns nil when the dependency is healthy.
type Dep struct {
	Name  string
	Check func(ctx context.Context) error
}

// Health serves the HA liveness/readiness/status endpoints.
//
//   - Live (/livez):  always 200 if the process is running. Liveness must not
//     depend on external services, or a Redis/DB blip would make k8s kill
//     otherwise-healthy pods.
//   - Ready (/readyz): 200 only when this instance is active (not draining) and
//     every dependency check passes; 503 otherwise. A load balancer uses this
//     to pull a draining or degraded instance out of rotation — which is what
//     makes graceful drain possible.
//   - Status (/ha/status): JSON snapshot of this instance, its role, and the
//     known cluster, for ops and the dashboard's Cluster panel.
//
// Note: the pre-existing /healthz (a dependency rollup) is intentionally left
// untouched for backward compatibility; these are additive endpoints.
type Health struct {
	registry *Registry
	version  string
	deps     []Dep
}

// NewHealth builds the handler. deps are the readiness dependency checks
// (e.g. database ping, and Redis ping when HA is enabled).
func NewHealth(registry *Registry, version string, deps ...Dep) *Health {
	return &Health{registry: registry, version: version, deps: deps}
}

// Live is the liveness probe: always 200 while the server is serving.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive"})
}

// Ready is the readiness probe.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	self := h.registry.Self()
	checks := make(map[string]string, len(h.deps))
	ready := true

	if self.Status != StatusActive {
		ready = false
	}
	for _, d := range h.deps {
		if err := d.Check(ctx); err != nil {
			ready = false
			checks[d.Name] = "down: " + err.Error()
			continue
		}
		checks[d.Name] = "ok"
	}

	status := "ready"
	code := http.StatusOK
	switch {
	case self.Status != StatusActive:
		status = "draining"
		code = http.StatusServiceUnavailable
	case !ready:
		status = "not_ready"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, map[string]any{
		"status":      status,
		"instance_id": self.ID,
		"checks":      checks,
	})
}

// Status is the ops/dashboard view of the cluster.
func (h *Health) Status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	self := h.registry.Self()
	out := map[string]any{
		"enabled":  h.registry.Enabled(),
		"version":  h.version,
		"instance": self,
		"role":     self.Status, // peers are equal; role reflects active/draining
	}

	instances, err := h.registry.Instances(ctx)
	if err != nil {
		// Redis hiccup: still report what we know about ourselves rather than
		// failing the status endpoint.
		out["instances"] = []Instance{self}
		out["instances_error"] = err.Error()
	} else {
		out["instances"] = instances
		active := 0
		for _, in := range instances {
			if in.Status == StatusActive {
				active++
			}
		}
		out["active_count"] = active
	}

	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
