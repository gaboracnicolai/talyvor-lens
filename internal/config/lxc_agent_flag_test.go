package config

import (
	"os"
	"testing"
)

// (proof 5) LXCAgentAllocationEnabled defaults TRUE (owner's call — the capstone ships armed). Setting the
// env to "false" turns it off (the caller then bypasses the sub-budget path).
func TestLXCAgentAllocation_DefaultsTrue(t *testing.T) {
	setRequiredEnv(t) // the base env Load() requires (redis/db/nats/api keys)
	// ensure the flag env isn't set for the default check.
	old, had := os.LookupEnv("LENS_LXC_AGENT_ALLOCATION_ENABLED")
	os.Unsetenv("LENS_LXC_AGENT_ALLOCATION_ENABLED")
	t.Cleanup(func() {
		if had {
			os.Setenv("LENS_LXC_AGENT_ALLOCATION_ENABLED", old)
		}
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.LXCAgentAllocationEnabled {
		t.Fatal("LXCAgentAllocationEnabled must DEFAULT TRUE (the capstone ships armed)")
	}

	t.Setenv("LENS_LXC_AGENT_ALLOCATION_ENABLED", "false")
	cfg2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.LXCAgentAllocationEnabled {
		t.Fatal("LENS_LXC_AGENT_ALLOCATION_ENABLED=false must turn it off")
	}
}
