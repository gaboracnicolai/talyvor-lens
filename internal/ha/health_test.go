package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func okDep(name string) Dep {
	return Dep{Name: name, Check: func(context.Context) error { return nil }}
}
func downDep(name string) Dep {
	return Dep{Name: name, Check: func(context.Context) error { return errors.New("unreachable") }}
}

func disabledRegistry() *Registry {
	return NewRegistry(nil, testInstance("solo", StatusActive, time.Now()), RegistryConfig{Enabled: false})
}

func call(h http.HandlerFunc) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

func TestHealth_LiveIsAlways200(t *testing.T) {
	// Even with a dead dependency and a draining instance, liveness is 200:
	// the process is up, k8s should not kill it.
	reg := disabledRegistry()
	_ = reg.SetDraining(context.Background())
	h := NewHealth(reg, "test", downDep("redis"))

	w := call(h.Live)
	if w.Code != http.StatusOK {
		t.Fatalf("Live status = %d, want 200", w.Code)
	}
}

func TestHealth_ReadyOKWhenActiveAndDepsUp(t *testing.T) {
	h := NewHealth(disabledRegistry(), "test", okDep("database"))
	w := call(h.Ready)
	if w.Code != http.StatusOK {
		t.Fatalf("Ready status = %d, want 200", w.Code)
	}
}

func TestHealth_Ready503WhenDraining(t *testing.T) {
	reg := disabledRegistry()
	if err := reg.SetDraining(context.Background()); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	h := NewHealth(reg, "test", okDep("database"))

	w := call(h.Ready)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready status = %d, want 503 while draining", w.Code)
	}
}

func TestHealth_Ready503WhenDependencyDown(t *testing.T) {
	h := NewHealth(disabledRegistry(), "test", okDep("database"), downDep("redis"))
	w := call(h.Ready)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Ready status = %d, want 503 when a dependency is down", w.Code)
	}
}

func TestHealth_StatusReportsEnabledFlagAndInstances(t *testing.T) {
	// Disabled: enabled=false, instances=[self].
	hd := NewHealth(disabledRegistry(), "test")
	var body map[string]any
	w := call(hd.Status)
	if w.Code != http.StatusOK {
		t.Fatalf("Status code = %d, want 200", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["enabled"] != false {
		t.Errorf("enabled = %v, want false", body["enabled"])
	}
	if insts, ok := body["instances"].([]any); !ok || len(insts) != 1 {
		t.Errorf("instances = %v, want exactly [self]", body["instances"])
	}

	// Enabled: enabled=true and self shows up after a heartbeat.
	reg, _ := enabledRegistry(t)
	if err := reg.Heartbeat(context.Background()); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	he := NewHealth(reg, "test")
	w = call(he.Status)
	body = nil
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["enabled"] != true {
		t.Errorf("enabled = %v, want true", body["enabled"])
	}
}
