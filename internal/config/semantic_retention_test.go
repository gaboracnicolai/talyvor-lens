package config

import (
	"testing"
	"time"
)

// B21.2: a stored answer is never deleted because of its age. With no environment variable set
// the retention window is 0 — served regardless of age and never swept.
func TestLoad_SemanticCacheRetentionDefaultsToKeepForever(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SemanticCacheRetention != 0 {
		t.Errorf("SemanticCacheRetention = %v with no env set, want 0 (kept until someone deletes it)", c.SemanticCacheRetention)
	}

	// A self-hosted operator can still turn the window on.
	t.Setenv("LENS_SEMANTIC_CACHE_RETENTION", "504h")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SemanticCacheRetention != 504*time.Hour {
		t.Errorf("SemanticCacheRetention = %v with LENS_SEMANTIC_CACHE_RETENTION=504h, want 504h", c.SemanticCacheRetention)
	}
}
