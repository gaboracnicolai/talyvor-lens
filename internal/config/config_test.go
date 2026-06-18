package config

import (
	"strings"
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

// TestLoad_WorkspaceReloadInterval — U7b: default 30s, parsed when set, and a
// non-positive value is rejected at load (a time.Ticker panics on it).
func TestLoad_WorkspaceReloadInterval(t *testing.T) {
	t.Run("default 30s", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.WorkspaceReloadInterval != 30*time.Second {
			t.Errorf("default = %v, want 30s", c.WorkspaceReloadInterval)
		}
	})
	t.Run("parsed when set", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_WORKSPACE_RELOAD_INTERVAL", "10s")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.WorkspaceReloadInterval != 10*time.Second {
			t.Errorf("= %v, want 10s", c.WorkspaceReloadInterval)
		}
	})
	for _, bad := range []string{"0s", "-5s", "nonsense"} {
		t.Run("rejected: "+bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_WORKSPACE_RELOAD_INTERVAL", bad)
			if _, err := Load(); err == nil {
				t.Errorf("LENS_WORKSPACE_RELOAD_INTERVAL=%q must be rejected at load", bad)
			}
		})
	}
}

// TestLoad_DBConns_Int32Bounds — the int32 pool-size knobs reject an out-of-int32
// value at load (no silent overflow on the int32 cast) and still parse a normal one.
func TestLoad_DBConns_Int32Bounds(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DBMaxConns != 25 || c.DBMinConns != 2 {
			t.Errorf("defaults = max %d / min %d, want 25 / 2", c.DBMaxConns, c.DBMinConns)
		}
	})
	t.Run("normal value parses", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_DB_MAX_CONNS", "100")
		t.Setenv("LENS_DB_MIN_CONNS", "5")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DBMaxConns != 100 || c.DBMinConns != 5 {
			t.Errorf("= max %d / min %d, want 100 / 5", c.DBMaxConns, c.DBMinConns)
		}
	})
	// Out-of-int32 must be rejected at load — NOT silently overflow the int32 cast.
	t.Run("rejected: max over int32", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_DB_MAX_CONNS", "9999999999") // > math.MaxInt32
		if _, err := Load(); err == nil {
			t.Error("LENS_DB_MAX_CONNS over int32 must be rejected (no silent overflow)")
		}
	})
	t.Run("rejected: min over int32", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_DB_MIN_CONNS", "2147483648") // math.MaxInt32 + 1
		if _, err := Load(); err == nil {
			t.Error("LENS_DB_MIN_CONNS over int32 must be rejected (no silent overflow)")
		}
	})
	for _, bad := range []string{"0", "-1", "nonsense"} {
		t.Run("rejected max: "+bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_DB_MAX_CONNS", bad)
			if _, err := Load(); err == nil {
				t.Errorf("LENS_DB_MAX_CONNS=%q must be rejected", bad)
			}
		})
	}
}

// TestLoad_GuardrailsReloadInterval — #189: default 30s, parsed when set, and a
// non-positive / unparseable value is rejected at load (a time.Ticker panics on it).
func TestLoad_GuardrailsReloadInterval(t *testing.T) {
	t.Run("default 30s", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.GuardrailsReloadInterval != 30*time.Second {
			t.Errorf("default = %v, want 30s", c.GuardrailsReloadInterval)
		}
	})
	t.Run("parsed when set", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_GUARDRAILS_RELOAD_INTERVAL", "15s")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.GuardrailsReloadInterval != 15*time.Second {
			t.Errorf("= %v, want 15s", c.GuardrailsReloadInterval)
		}
	})
	for _, bad := range []string{"0s", "-5s", "nonsense"} {
		t.Run("rejected: "+bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_GUARDRAILS_RELOAD_INTERVAL", bad)
			if _, err := Load(); err == nil {
				t.Errorf("LENS_GUARDRAILS_RELOAD_INTERVAL=%q must be rejected at load", bad)
			}
		})
	}
}

// TestLoad_DBReplicaURL — U8/U9: optional read-replica DSN, default empty (off).
// Load does NOT validate the URL — a malformed value falls back to the primary
// with a WARN at boot (dbrouting.OpenReplica), never a crash — so Load only
// stores the raw string here.
// TestLoad_WorkTierEnabled — capability flag, default false; survives the economy
// kill (descriptive, NOT a safety restriction — must NOT be in the force-off block).
func TestLoad_WorkTierEnabled(t *testing.T) {
	t.Run("default false", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.WorkTierEnabled {
			t.Error("WorkTierEnabled must default false")
		}
	})
	t.Run("parsed true", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_WORKTIER_ENABLED", "true")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !c.WorkTierEnabled {
			t.Error("LENS_WORKTIER_ENABLED=true must enable it")
		}
	})
	t.Run("survives the economy kill", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_WORKTIER_ENABLED", "true")
		t.Setenv("LENS_ECONOMY_ENABLED", "false")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !c.WorkTierEnabled {
			t.Error("WorkTier is a descriptive capability flag — it must NOT be force-off'd by the economy kill")
		}
	})
}

// TestLoad_MintRateCapLENS24h — U6 PR2: real default 1000 (default-ON ceiling),
// parsed when set, 0 valid (disables), negative rejected.
func TestLoad_MintRateCapLENS24h(t *testing.T) {
	t.Run("default 1000", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.MintRateCapLENS24h != 1000 {
			t.Errorf("default = %v, want 1000 (default-ON ceiling)", c.MintRateCapLENS24h)
		}
	})
	t.Run("parsed when set", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_MINT_RATE_CAP_LENS_24H", "250.5")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.MintRateCapLENS24h != 250.5 {
			t.Errorf("= %v, want 250.5", c.MintRateCapLENS24h)
		}
	})
	t.Run("zero disables (valid)", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_MINT_RATE_CAP_LENS_24H", "0")
		c, err := Load()
		if err != nil {
			t.Fatalf("0 must be valid (explicit disable): %v", err)
		}
		if c.MintRateCapLENS24h != 0 {
			t.Errorf("= %v, want 0 (off)", c.MintRateCapLENS24h)
		}
	})
	t.Run("negative rejected", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_MINT_RATE_CAP_LENS_24H", "-5")
		if _, err := Load(); err == nil {
			t.Error("a negative cap must be rejected")
		}
	})
}

func TestLoad_DBReplicaURL(t *testing.T) {
	t.Run("default empty (off)", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DBReplicaURL != "" {
			t.Errorf("DBReplicaURL default must be empty (off), got %q", c.DBReplicaURL)
		}
	})
	t.Run("stored when set", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_DB_REPLICA_URL", "postgres://replica.internal:5432/lens?sslmode=disable")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.DBReplicaURL != "postgres://replica.internal:5432/lens?sslmode=disable" {
			t.Errorf("DBReplicaURL = %q", c.DBReplicaURL)
		}
	})
}

// TestLoad_Audit — U14 audit knobs default OFF; parse when set; reject a
// non-positive export interval (a time.Ticker panics on it).
func TestLoad_Audit(t *testing.T) {
	t.Run("defaults: all off", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.AuditRetention != 0 {
			t.Errorf("AuditRetention default must be 0 (disabled), got %v", c.AuditRetention)
		}
		if c.AuditExportURL != "" {
			t.Errorf("AuditExportURL default must be empty (off), got %q", c.AuditExportURL)
		}
		if c.AuditExportInterval != time.Hour {
			t.Errorf("AuditExportInterval default must be 1h, got %v", c.AuditExportInterval)
		}
	})
	t.Run("parsed when set", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_AUDIT_RETENTION", "8760h")
		t.Setenv("LENS_AUDIT_EXPORT_URL", "https://siem.example/ingest")
		t.Setenv("LENS_AUDIT_EXPORT_INTERVAL", "15m")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if c.AuditRetention != 8760*time.Hour {
			t.Errorf("AuditRetention = %v, want 8760h", c.AuditRetention)
		}
		if c.AuditExportURL != "https://siem.example/ingest" {
			t.Errorf("AuditExportURL = %q", c.AuditExportURL)
		}
		if c.AuditExportInterval != 15*time.Minute {
			t.Errorf("AuditExportInterval = %v, want 15m", c.AuditExportInterval)
		}
	})
	t.Run("non-positive export interval rejected", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_AUDIT_EXPORT_INTERVAL", "0s")
		if _, err := Load(); err == nil {
			t.Error("a non-positive LENS_AUDIT_EXPORT_INTERVAL must fail Load (ticker panics)")
		}
	})
}

// TestLoad_Billing_FailsWhenEnabledWithoutKeys — billing moves fiat money, so an
// enabled-but-half-configured deployment must refuse to start. Disabled (default)
// ignores the keys; enabled requires BOTH; the error never echoes a secret value.
func TestLoad_Billing_FailsWhenEnabledWithoutKeys(t *testing.T) {
	t.Run("disabled: keys optional, default off", func(t *testing.T) {
		setRequiredEnv(t)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load (billing default-off): %v", err)
		}
		if c.BillingEnabled {
			t.Error("BillingEnabled must default false")
		}
	})
	t.Run("enabled, no keys: startup fails", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_BILLING_ENABLED", "true")
		if _, err := Load(); err == nil {
			t.Fatal("billing enabled without Stripe keys must fail Load")
		}
	})
	t.Run("enabled, only secret key: startup fails", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_BILLING_ENABLED", "true")
		t.Setenv("LENS_STRIPE_SECRET_KEY", "sk_test_x")
		if _, err := Load(); err == nil {
			t.Fatal("billing enabled with the webhook secret missing must fail Load")
		}
	})
	t.Run("enabled, both keys: ok", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_BILLING_ENABLED", "true")
		t.Setenv("LENS_STRIPE_SECRET_KEY", "sk_test_x")
		t.Setenv("LENS_STRIPE_WEBHOOK_SECRET", "whsec_x")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load (billing fully configured): %v", err)
		}
		if !c.BillingEnabled || c.StripeSecretKey == "" || c.StripeWebhookSecret == "" {
			t.Error("fully-configured billing must load with fields set")
		}
	})
	t.Run("startup error names env vars, not the secret value", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("LENS_BILLING_ENABLED", "true")
		t.Setenv("LENS_STRIPE_SECRET_KEY", "sk_live_SUPERSECRET")
		_, err := Load()
		if err == nil {
			t.Fatal("expected failure")
		}
		if strings.Contains(err.Error(), "SUPERSECRET") {
			t.Errorf("startup error must not echo the secret value: %v", err)
		}
	})
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

func TestLoad_NodeTLSSkipVerifyDefaultsOff(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.NodeTLSSkipVerify {
		t.Error("NodeTLSSkipVerify should default to false")
	}
}

func TestLoad_NodeTLSSkipVerifyEnabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_NODE_TLS_SKIP_VERIFY", "true")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.NodeTLSSkipVerify {
		t.Error("NodeTLSSkipVerify should be true when LENS_NODE_TLS_SKIP_VERIFY=true")
	}
}

func TestLoad_PoolRoyaltyDefaults(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PoolRoyaltyMintingEnabled {
		t.Error("PoolRoyaltyMintingEnabled should default to false (Stage 2.1 is inert by default)")
	}
	if c.PoolRoyaltyShare != 0.5 {
		t.Errorf("PoolRoyaltyShare = %v, want 0.5 (DefaultRoyaltyShare)", c.PoolRoyaltyShare)
	}
}

func TestLoad_PoolRoyaltyParsing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_POOL_ROYALTY_MINTING_ENABLED", "true")
	t.Setenv("LENS_POOL_ROYALTY_SHARE", "0.7")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.PoolRoyaltyMintingEnabled {
		t.Error("PoolRoyaltyMintingEnabled should be true when LENS_POOL_ROYALTY_MINTING_ENABLED=true")
	}
	if c.PoolRoyaltyShare != 0.7 {
		t.Errorf("PoolRoyaltyShare = %v, want 0.7", c.PoolRoyaltyShare)
	}
}

func TestLoad_PoolRoyaltyShareInvalidRejected(t *testing.T) {
	for _, bad := range []string{"1.5", "-0.1", "abc"} {
		t.Run(bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_POOL_ROYALTY_SHARE", bad)
			if _, err := Load(); err == nil {
				t.Errorf("Load should reject LENS_POOL_ROYALTY_SHARE=%q (must be in [0,1]); Talyvor net (1−s)×avoided_COGS must stay ≥ 0", bad)
			}
		})
	}
}

// NaN parses without error and compares false to every bound — the [0,1]
// validation must reject it explicitly or a NaN share reaches the mint math.
func TestLoad_PoolRoyaltyShareNaNRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_POOL_ROYALTY_SHARE", "NaN")
	if _, err := Load(); err == nil {
		t.Error("Load must reject LENS_POOL_ROYALTY_SHARE=NaN (it bypasses range comparisons and corrupts balances as NaN×COGS)")
	}
}

func TestLoad_PoolMintCapDefaults(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PoolMintCapPerPair != 0 {
		t.Errorf("PoolMintCapPerPair = %d, want 0 (cap disabled by default — opt-in)", c.PoolMintCapPerPair)
	}
	if c.PoolMintCapWindow != 24*time.Hour {
		t.Errorf("PoolMintCapWindow = %v, want 24h default", c.PoolMintCapWindow)
	}
}

func TestLoad_PoolMintCapParsing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_POOL_MINT_CAP_PER_PAIR", "500")
	t.Setenv("LENS_POOL_MINT_CAP_WINDOW", "48h")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PoolMintCapPerPair != 500 || c.PoolMintCapWindow != 48*time.Hour {
		t.Errorf("got cap=%d window=%v, want 500/48h", c.PoolMintCapPerPair, c.PoolMintCapWindow)
	}
}

func TestLoad_PoolMintCapInvalidRejected(t *testing.T) {
	for name, env := range map[string][2]string{
		"negative cap":    {"LENS_POOL_MINT_CAP_PER_PAIR", "-1"},
		"non-numeric cap": {"LENS_POOL_MINT_CAP_PER_PAIR", "many"},
		"zero window":     {"LENS_POOL_MINT_CAP_WINDOW", "0s"},
		"negative window": {"LENS_POOL_MINT_CAP_WINDOW", "-1h"},
		"bad window":      {"LENS_POOL_MINT_CAP_WINDOW", "fortnight"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(); err == nil {
				t.Errorf("Load must reject %s=%q", env[0], env[1])
			}
		})
	}
}

func TestLoad_PoolHoldbackWindowDefaultAndParsing(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PoolHoldbackWindow != 72*time.Hour {
		t.Errorf("PoolHoldbackWindow = %v, want 72h default", c.PoolHoldbackWindow)
	}
	t.Setenv("LENS_POOL_HOLDBACK_WINDOW", "48h")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.PoolHoldbackWindow != 48*time.Hour {
		t.Errorf("PoolHoldbackWindow = %v, want 48h", c2.PoolHoldbackWindow)
	}
}

func TestLoad_PoolHoldbackWindowInvalidRejected(t *testing.T) {
	for _, bad := range []string{"0s", "-1h", "tomorrow"} {
		t.Run(bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_POOL_HOLDBACK_WINDOW", bad)
			if _, err := Load(); err == nil {
				t.Errorf("Load must reject LENS_POOL_HOLDBACK_WINDOW=%q", bad)
			}
		})
	}
}

func TestLoad_DetectorThresholdDefaults(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DetectVolumeMinMints != 50 || c.DetectVolumeMaxRequesters != 2 {
		t.Errorf("volume defaults = %d/%d, want 50/2", c.DetectVolumeMinMints, c.DetectVolumeMaxRequesters)
	}
	if c.DetectBilateralMinFrac != 0.9 || c.DetectBilateralMinMints != 20 {
		t.Errorf("bilateral defaults = %v/%d, want 0.9/20", c.DetectBilateralMinFrac, c.DetectBilateralMinMints)
	}
	if c.DetectSimilarityMinSample != 30 || c.DetectSimilarityMaxStddev != 0.02 {
		t.Errorf("similarity defaults = %d/%v, want 30/0.02", c.DetectSimilarityMinSample, c.DetectSimilarityMaxStddev)
	}
}

func TestLoad_DetectorThresholdParsing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_DETECT_VOLUME_MIN_MINTS", "100")
	t.Setenv("LENS_DETECT_VOLUME_MAX_REQUESTERS", "3")
	t.Setenv("LENS_DETECT_BILATERAL_MIN_FRAC", "0.95")
	t.Setenv("LENS_DETECT_BILATERAL_MIN_MINTS", "40")
	t.Setenv("LENS_DETECT_SIMILARITY_MIN_SAMPLE", "50")
	t.Setenv("LENS_DETECT_SIMILARITY_MAX_STDDEV", "0.01")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DetectVolumeMinMints != 100 || c.DetectVolumeMaxRequesters != 3 ||
		c.DetectBilateralMinFrac != 0.95 || c.DetectBilateralMinMints != 40 ||
		c.DetectSimilarityMinSample != 50 || c.DetectSimilarityMaxStddev != 0.01 {
		t.Errorf("parsed = %+v", c)
	}
}

func TestLoad_DetectorThresholdInvalidRejected(t *testing.T) {
	for name, env := range map[string][2]string{
		"neg volume mints":   {"LENS_DETECT_VOLUME_MIN_MINTS", "-1"},
		"bad volume mints":   {"LENS_DETECT_VOLUME_MIN_MINTS", "lots"},
		"neg max requesters": {"LENS_DETECT_VOLUME_MAX_REQUESTERS", "-1"},
		"frac > 1":           {"LENS_DETECT_BILATERAL_MIN_FRAC", "1.5"},
		"frac NaN":           {"LENS_DETECT_BILATERAL_MIN_FRAC", "NaN"},
		"frac negative":      {"LENS_DETECT_BILATERAL_MIN_FRAC", "-0.1"},
		"min sample < 1":     {"LENS_DETECT_SIMILARITY_MIN_SAMPLE", "0"},
		"stddev negative":    {"LENS_DETECT_SIMILARITY_MAX_STDDEV", "-0.01"},
		"stddev NaN":         {"LENS_DETECT_SIMILARITY_MAX_STDDEV", "NaN"},
	} {
		t.Run(name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(env[0], env[1])
			if _, err := Load(); err == nil {
				t.Errorf("Load must reject %s=%q", env[0], env[1])
			}
		})
	}
}

func TestLoad_PoolMintCapPerEntry(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PoolMintCapPerEntry != 0 {
		t.Errorf("PoolMintCapPerEntry default = %d, want 0 (disabled)", c.PoolMintCapPerEntry)
	}
	t.Setenv("LENS_POOL_MINT_CAP_PER_ENTRY", "200")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.PoolMintCapPerEntry != 200 {
		t.Errorf("PoolMintCapPerEntry = %d, want 200", c2.PoolMintCapPerEntry)
	}
}

func TestLoad_PoolMintCapPerEntryInvalidRejected(t *testing.T) {
	for _, bad := range []string{"-1", "lots"} {
		t.Run(bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_POOL_MINT_CAP_PER_ENTRY", bad)
			if _, err := Load(); err == nil {
				t.Errorf("Load must reject LENS_POOL_MINT_CAP_PER_ENTRY=%q", bad)
			}
		})
	}
}

func TestLoad_LXCShadowSpendEnabledDefaultsOffAndParses(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LXCShadowSpendEnabled {
		t.Error("LXCShadowSpendEnabled must DEFAULT FALSE (first live-serving-path change — inert until deliberately enabled)")
	}
	t.Setenv("LENS_LXC_SHADOW_SPEND_ENABLED", "true")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c2.LXCShadowSpendEnabled {
		t.Error("LENS_LXC_SHADOW_SPEND_ENABLED=true must enable the flag")
	}
}

func TestLoad_LXCGatingEnabledDefaultsOffAndParses(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LXCGatingEnabled {
		t.Error("LXCGatingEnabled must DEFAULT FALSE (the live-path block — inert until deliberately enabled)")
	}
	t.Setenv("LENS_LXC_GATING_ENABLED", "true")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c2.LXCGatingEnabled {
		t.Error("LENS_LXC_GATING_ENABLED=true must enable the flag")
	}
}

func TestLoad_PatternCaptureEnabledDefaultsOffAndParses(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PatternCaptureEnabled {
		t.Error("PatternCaptureEnabled must DEFAULT FALSE (capture is inert until deliberately enabled)")
	}
	t.Setenv("LENS_PATTERN_CAPTURE_ENABLED", "true")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c2.PatternCaptureEnabled {
		t.Error("LENS_PATTERN_CAPTURE_ENABLED=true must enable the flag")
	}
}

func TestLoad_PatternEarnCapDefaultsAndParses(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PatternEarnCapPerWorkspace != 50000 {
		t.Errorf("PatternEarnCapPerWorkspace must DEFAULT 50000 (real cap, not 0); got %d", c.PatternEarnCapPerWorkspace)
	}
	if c.PatternEarnCapWindow != 24*time.Hour {
		t.Errorf("PatternEarnCapWindow must DEFAULT 24h; got %v", c.PatternEarnCapWindow)
	}
	t.Setenv("LENS_PATTERN_EARN_CAP_PER_WORKSPACE", "100")
	t.Setenv("LENS_PATTERN_EARN_CAP_WINDOW", "48h")
	c2, _ := Load()
	if c2.PatternEarnCapPerWorkspace != 100 || c2.PatternEarnCapWindow != 48*time.Hour {
		t.Errorf("env overrides not applied: %d / %v", c2.PatternEarnCapPerWorkspace, c2.PatternEarnCapWindow)
	}
}

func TestLoad_PatternEarningEnabledDefaultsOffAndParses(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PatternEarningEnabled {
		t.Error("PatternEarningEnabled must DEFAULT FALSE (earning is the live-path flip — inert until enabled)")
	}
	t.Setenv("LENS_PATTERN_EARNING_ENABLED", "true")
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c2.PatternEarningEnabled {
		t.Error("LENS_PATTERN_EARNING_ENABLED=true must enable the flag")
	}
}
