package config

import (
	"os"
	"path/filepath"
	"testing"
)

// An explicit LENS_DISTILL_WORKER_BIN wins — operators can point at a custom
// deployment path.
func TestLoad_DistillWorkerBin_EnvOverride(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_DISTILL_WORKER_BIN", "/opt/lens/bin/distill-worker")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DistillWorkerBin != "/opt/lens/bin/distill-worker" {
		t.Errorf("DistillWorkerBin = %q, want the explicit override", c.DistillWorkerBin)
	}
}

// With no env set, the worker path defaults to the distill-worker binary BESIDE
// the running executable — so the Docker image (lens + distill-worker in the
// same dir) works out of the box with zero config.
func TestLoad_DistillWorkerBin_DefaultBesideLens(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if filepath.Base(c.DistillWorkerBin) != "distill-worker" {
		t.Errorf("default worker bin should be named distill-worker; got %q", c.DistillWorkerBin)
	}
	if exe, err := os.Executable(); err == nil {
		if got, want := filepath.Dir(c.DistillWorkerBin), filepath.Dir(exe); got != want {
			t.Errorf("default worker dir = %q, want beside the running binary %q", got, want)
		}
	}
}
