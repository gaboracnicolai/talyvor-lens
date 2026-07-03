package config

import "testing"

// F4 step B: the gateway price ceiling defaults to 0.50 (10× the 0.050 node band — legitimate nodes never
// clamped, only inflated declarations). An env override must be > 0.
func TestGatewayPriceCeiling_Default(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GatewayPriceCeiling != 0.50 {
		t.Fatalf("GatewayPriceCeiling default must be 0.50, got %v", cfg.GatewayPriceCeiling)
	}

	t.Setenv("LENS_GATEWAY_PRICE_CEILING", "1.25")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.GatewayPriceCeiling != 1.25 {
		t.Fatalf("override must apply, got %v", cfg2.GatewayPriceCeiling)
	}

	t.Setenv("LENS_GATEWAY_PRICE_CEILING", "0")
	if _, err := Load(); err == nil {
		t.Fatal("ceiling ≤ 0 must be rejected")
	}
}
