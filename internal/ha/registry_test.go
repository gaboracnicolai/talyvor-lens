package ha

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func enabledRegistry(t *testing.T) (*Registry, *miniRedisHandle) {
	t.Helper()
	rc, mr := newTestRedis(t)
	self := testInstance("self-1", StatusActive, time.Now())
	reg := NewRegistry(rc, self, RegistryConfig{
		Enabled:        true,
		TTL:            15 * time.Second,
		HeartbeatEvery: 5 * time.Second,
	})
	return reg, &miniRedisHandle{rc: rc, mr: mr}
}

type miniRedisHandle struct {
	rc *redis.Client
	mr *miniredis.Miniredis
}

func TestRegistry_HeartbeatWritesKeyWithTTL(t *testing.T) {
	reg, h := enabledRegistry(t)
	ctx := context.Background()

	if err := reg.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	key := "ha:instance:self-1"
	if !h.mr.Exists(key) {
		t.Fatalf("expected key %q to exist after heartbeat", key)
	}
	ttl := h.mr.TTL(key)
	if ttl <= 0 || ttl > 15*time.Second {
		t.Fatalf("TTL = %v, want in (0, 15s]", ttl)
	}

	// After the TTL elapses with no refresh, the key disappears — a crashed
	// instance vanishes from the registry.
	h.mr.FastForward(16 * time.Second)
	if h.mr.Exists(key) {
		t.Fatal("expected key to expire after TTL with no heartbeat")
	}
}

func TestRegistry_ActiveInstancesFiltersStaleAndDraining(t *testing.T) {
	reg, h := enabledRegistry(t)
	ctx := context.Background()

	// self: active + fresh (written via heartbeat)
	if err := reg.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	// peer-active: another healthy instance
	writeInstance(t, h, testInstance("peer-active", StatusActive, time.Now()))
	// peer-draining: should be excluded from "active for routing"
	writeInstance(t, h, testInstance("peer-draining", StatusDraining, time.Now()))
	// peer-stale: active status but LastSeen is ancient → excluded
	writeInstance(t, h, testInstance("peer-stale", StatusActive, time.Now().Add(-time.Hour)))

	active, err := reg.ActiveInstances(ctx)
	if err != nil {
		t.Fatalf("ActiveInstances: %v", err)
	}
	got := idSet(active)
	if !got["self-1"] || !got["peer-active"] {
		t.Fatalf("expected self-1 and peer-active in active set, got %v", got)
	}
	if got["peer-draining"] {
		t.Error("draining instance must be excluded from active set")
	}
	if got["peer-stale"] {
		t.Error("stale instance must be excluded from active set")
	}
}

func TestRegistry_SetDrainingExcludesSelfFromActive(t *testing.T) {
	reg, h := enabledRegistry(t)
	ctx := context.Background()
	if err := reg.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	writeInstance(t, h, testInstance("peer-active", StatusActive, time.Now()))

	if err := reg.SetDraining(ctx); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	if reg.Self().Status != StatusDraining {
		t.Fatalf("self status = %q, want draining", reg.Self().Status)
	}

	active, err := reg.ActiveInstances(ctx)
	if err != nil {
		t.Fatalf("ActiveInstances: %v", err)
	}
	if idSet(active)["self-1"] {
		t.Error("a draining instance must not appear in its own active set")
	}
}

func TestRegistry_DeregisterRemovesKey(t *testing.T) {
	reg, h := enabledRegistry(t)
	ctx := context.Background()
	if err := reg.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !h.mr.Exists("ha:instance:self-1") {
		t.Fatal("precondition: key should exist")
	}
	if err := reg.Deregister(ctx); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if h.mr.Exists("ha:instance:self-1") {
		t.Error("Deregister should DEL the instance key")
	}
}

func TestRegistry_DisabledIsNoOpAndReturnsSelf(t *testing.T) {
	// Disabled registry with a nil client: methods must not touch Redis.
	self := testInstance("solo", StatusActive, time.Now())
	reg := NewRegistry(nil, self, RegistryConfig{Enabled: false})
	ctx := context.Background()

	if err := reg.Heartbeat(ctx); err != nil {
		t.Errorf("disabled Heartbeat should be a nil no-op, got %v", err)
	}
	if err := reg.Deregister(ctx); err != nil {
		t.Errorf("disabled Deregister should be a nil no-op, got %v", err)
	}
	active, err := reg.ActiveInstances(ctx)
	if err != nil {
		t.Fatalf("disabled ActiveInstances: %v", err)
	}
	if len(active) != 1 || active[0].ID != "solo" {
		t.Fatalf("disabled ActiveInstances = %v, want [self]", active)
	}
}

// --- helpers ---

func writeInstance(t *testing.T, h *miniRedisHandle, in Instance) {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := h.rc.Set(context.Background(), "ha:instance:"+in.ID, data, 15*time.Second).Err(); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
}

func idSet(insts []Instance) map[string]bool {
	m := map[string]bool{}
	for _, in := range insts {
		m[in.ID] = true
	}
	return m
}
