package config

import (
	"testing"
	"time"
)

// setRequiredEnv sets the env vars Load() requires so tests can focus on the
// fields under test.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LENS_REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("LENS_DATABASE_URL", "postgres://localhost:5432/lens")
	t.Setenv("LENS_NATS_URL", "nats://localhost:4222")
	t.Setenv("LENS_OPENAI_API_KEY", "sk-test")
	t.Setenv("LENS_ANTHROPIC_API_KEY", "sk-ant-test")
}

func TestLoad_HADefaults(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HAEnabled {
		t.Error("HAEnabled should default to false (HA is opt-in)")
	}
	if c.HAHeartbeat != 5*time.Second {
		t.Errorf("HAHeartbeat = %v, want 5s", c.HAHeartbeat)
	}
	if c.HAInstanceTTL != 15*time.Second {
		t.Errorf("HAInstanceTTL = %v, want 15s", c.HAInstanceTTL)
	}
	if c.HADrainTimeout != 30*time.Second {
		t.Errorf("HADrainTimeout = %v, want 30s", c.HADrainTimeout)
	}
}

func TestLoad_HAEnabledParsing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_HA_ENABLED", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.HAEnabled {
		t.Error("HAEnabled should be true when LENS_HA_ENABLED=true")
	}
}

func TestLoad_HAOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_HA_HEARTBEAT_SEC", "3")
	t.Setenv("LENS_HA_INSTANCE_TTL_SEC", "20")
	t.Setenv("LENS_HA_DRAIN_TIMEOUT_SEC", "45")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.HAHeartbeat != 3*time.Second {
		t.Errorf("HAHeartbeat = %v, want 3s", c.HAHeartbeat)
	}
	if c.HAInstanceTTL != 20*time.Second {
		t.Errorf("HAInstanceTTL = %v, want 20s", c.HAInstanceTTL)
	}
	if c.HADrainTimeout != 45*time.Second {
		t.Errorf("HADrainTimeout = %v, want 45s", c.HADrainTimeout)
	}
}

func TestLoad_HAInvalidValueRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_HA_INSTANCE_TTL_SEC", "0")
	if _, err := Load(); err == nil {
		t.Error("expected error for LENS_HA_INSTANCE_TTL_SEC=0")
	}
}
