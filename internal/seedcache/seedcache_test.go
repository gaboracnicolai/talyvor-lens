package seedcache

import (
	"testing"

	"github.com/talyvor/lens/internal/economy"
)

// The owner stamped on every seed is the dedicated, never-verified platform workspace — hardcoded,
// so the tool can never write a seed under a tenant id or the fee workspace. This pins the single
// fact the zero-mint guarantee rests on.
func TestOwner_IsTheDedicatedSeedWorkspace(t *testing.T) {
	if Owner != economy.TalyvorSeedWorkspace {
		t.Fatalf("seed Owner %q must be economy.TalyvorSeedWorkspace", Owner)
	}
	if Owner != "talyvor-seed" {
		t.Fatalf("seed Owner %q, want talyvor-seed", Owner)
	}
	if Owner == economy.TalyvorWorkspace {
		t.Fatal("seed Owner must be DISTINCT from the marketplace-fee workspace (TalyvorWorkspace)")
	}
}
