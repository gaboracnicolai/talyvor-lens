package main

import (
	"crypto/tls"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestApplyRedisTLS_NilConfig verifies that a redis:// URL (no TLSConfig)
// gets a new TLS 1.2 config when applyRedisTLS is called.
func TestApplyRedisTLS_NilConfig(t *testing.T) {
	opts := &redis.Options{}
	applyRedisTLS(opts, false)

	if opts.TLSConfig == nil {
		t.Fatal("expected TLSConfig to be set after applyRedisTLS")
	}
	if opts.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2 (%d)", opts.TLSConfig.MinVersion, tls.VersionTLS12)
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be false when skipVerify=false")
	}
}

// TestApplyRedisTLS_SkipVerify verifies that skipVerify=true sets
// InsecureSkipVerify on the resulting TLS config.
func TestApplyRedisTLS_SkipVerify(t *testing.T) {
	opts := &redis.Options{}
	applyRedisTLS(opts, true)

	if opts.TLSConfig == nil {
		t.Fatal("expected TLSConfig to be set")
	}
	if !opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true when skipVerify=true")
	}
}

// TestApplyRedisTLS_PreservesExistingConfig verifies that when the operator
// supplies a rediss:// URL (go-redis sets TLSConfig), our function does NOT
// replace their config — it only layers in InsecureSkipVerify.
func TestApplyRedisTLS_PreservesExistingConfig(t *testing.T) {
	existing := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: "redis.example.com",
	}
	opts := &redis.Options{TLSConfig: existing}
	applyRedisTLS(opts, false)

	if opts.TLSConfig.MinVersion != tls.VersionTLS13 {
		t.Error("existing TLSConfig MinVersion should be preserved")
	}
	if opts.TLSConfig.ServerName != "redis.example.com" {
		t.Error("existing TLSConfig ServerName should be preserved")
	}
	if opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should remain false")
	}
}

// TestApplyRedisTLS_RedisSScheme confirms that go-redis ParseURL sets
// TLSConfig for a rediss:// URL, and that applyRedisTLS preserves it while
// correctly applying the skip-verify flag.
func TestApplyRedisTLS_RedisSScheme(t *testing.T) {
	opts, err := redis.ParseURL("rediss://localhost:6380")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	if opts.TLSConfig == nil {
		t.Fatal("expected rediss:// URL to set TLSConfig via ParseURL")
	}

	applyRedisTLS(opts, false)
	if opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should remain false after applyRedisTLS(false)")
	}

	applyRedisTLS(opts, true)
	if !opts.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true after applyRedisTLS(true)")
	}
}
