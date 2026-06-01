package controlplane

import (
	"context"
	"testing"
	"time"
)

// ─── HeartbeatStore ──────────────────────────────────────────────────────────

func TestHeartbeatStore_RecordAndIsFresh(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	if err := hb.Record(context.Background(), "inference", "node-1", 300); err != nil {
		t.Fatalf("Record: %v", err)
	}
	fresh, ts, err := hb.IsFresh(context.Background(), "inference", "node-1")
	if err != nil {
		t.Fatalf("IsFresh: %v", err)
	}
	if !fresh {
		t.Fatal("expected fresh heartbeat immediately after Record")
	}
	if ts.IsZero() {
		t.Fatal("expected non-zero timestamp")
	}
	if time.Since(ts) > 5*time.Second {
		t.Fatalf("timestamp too old: %v", ts)
	}
}

func TestHeartbeatStore_MissingKey_ReturnsFalse(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	fresh, ts, err := hb.IsFresh(context.Background(), "cache", "no-such-node")
	if err != nil {
		t.Fatalf("IsFresh on missing key should not error, got: %v", err)
	}
	if fresh {
		t.Fatal("expected false for missing key")
	}
	if !ts.IsZero() {
		t.Fatalf("expected zero timestamp for missing key, got %v", ts)
	}
}

func TestHeartbeatStore_NilRedis_NoOp(t *testing.T) {
	hb := NewHeartbeatStore(nil)

	if err := hb.Record(context.Background(), "embedding", "node-x", 60); err != nil {
		t.Fatalf("Record with nil redis should not error, got: %v", err)
	}
	fresh, ts, err := hb.IsFresh(context.Background(), "embedding", "node-x")
	if err != nil {
		t.Fatalf("IsFresh with nil redis should not error, got: %v", err)
	}
	if fresh {
		t.Fatal("expected false when redis is nil")
	}
	if !ts.IsZero() {
		t.Fatalf("expected zero timestamp when redis is nil, got %v", ts)
	}
}

func TestHeartbeatStore_TTLIsSet(t *testing.T) {
	rc, mr := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	if err := hb.Record(context.Background(), "cache", "node-ttl", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	key := hbKey("cache", "node-ttl")
	if ttl := mr.TTL(key); ttl <= 0 {
		t.Fatalf("expected TTL > 0 on heartbeat key, got %v", ttl)
	}
}

func TestHeartbeatStore_KeyExpiry_ReturnsFalse(t *testing.T) {
	rc, mr := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	if err := hb.Record(context.Background(), "inference", "node-exp", 100); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Force the key to expire in miniredis.
	mr.FastForward(hbTTL + time.Second)

	fresh, _, err := hb.IsFresh(context.Background(), "inference", "node-exp")
	if err != nil {
		t.Fatalf("IsFresh after expiry should not error, got: %v", err)
	}
	if fresh {
		t.Fatal("expected false after key expiry")
	}
}

func TestHeartbeatStore_DifferentTypesAreIndependent(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	// Record only for "inference" type.
	if err := hb.Record(context.Background(), "inference", "shared-id", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// "cache" and "embedding" with the same ID should NOT be fresh.
	for _, typ := range []string{"cache", "embedding"} {
		fresh, _, err := hb.IsFresh(context.Background(), typ, "shared-id")
		if err != nil {
			t.Fatalf("IsFresh(%s): %v", typ, err)
		}
		if fresh {
			t.Fatalf("expected %s/shared-id to be absent (only inference was recorded)", typ)
		}
	}
	// But inference should still be fresh.
	fresh, _, err := hb.IsFresh(context.Background(), "inference", "shared-id")
	if err != nil {
		t.Fatalf("IsFresh(inference): %v", err)
	}
	if !fresh {
		t.Fatal("expected inference/shared-id to be fresh")
	}
}
