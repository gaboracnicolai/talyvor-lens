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

func TestLoad_DBSSLModeDefault(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DBSSLMode != "require" {
		t.Errorf("DBSSLMode default = %q, want %q", c.DBSSLMode, "require")
	}
}

func TestLoad_DBSSLModeOverride(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_DB_SSL_MODE", "verify-full")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DBSSLMode != "verify-full" {
		t.Errorf("DBSSLMode = %q, want %q", c.DBSSLMode, "verify-full")
	}
}

func TestLoad_DBSSLModeInvalidRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_DB_SSL_MODE", "bogus")
	if _, err := Load(); err == nil {
		t.Error("expected error for LENS_DB_SSL_MODE=bogus")
	}
}

func TestLoad_DBSSLModeDisableAllowed(t *testing.T) {
	// "disable" is a valid (if insecure) value — Load must accept it.
	setRequiredEnv(t)
	t.Setenv("LENS_DB_SSL_MODE", "disable")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DBSSLMode != "disable" {
		t.Errorf("DBSSLMode = %q, want %q", c.DBSSLMode, "disable")
	}
}

func TestLoad_NatsTLSDefaultsOff(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.NatsTLS {
		t.Error("NatsTLS should default to false")
	}
	if c.NatsTLSSkipVerify {
		t.Error("NatsTLSSkipVerify should default to false")
	}
}

func TestLoad_NatsTLSEnabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_NATS_TLS", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.NatsTLS {
		t.Error("NatsTLS should be true when LENS_NATS_TLS=true")
	}
}

func TestLoad_NatsTLSSkipVerifyEnabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_NATS_TLS", "true")
	t.Setenv("LENS_NATS_TLS_SKIP_VERIFY", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.NatsTLS {
		t.Error("NatsTLS should be true")
	}
	if !c.NatsTLSSkipVerify {
		t.Error("NatsTLSSkipVerify should be true when LENS_NATS_TLS_SKIP_VERIFY=true")
	}
}
