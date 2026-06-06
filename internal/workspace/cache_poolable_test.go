package workspace

import (
	"context"
	"sync"
	"testing"
)

// Private-by-default: an unregistered or unset workspace is never poolable, so
// cross-tenant cache pooling can never turn on implicitly.
func TestCachePoolable_DefaultFalse(t *testing.T) {
	m := New(nil)
	if m.GetCachePoolable("unknown") {
		t.Error("an unregistered workspace must be non-poolable (private by default)")
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if m.GetCachePoolable("w") {
		t.Error("a workspace with no flag set must default to non-poolable")
	}
}

func TestSetCachePoolable_InMemory(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	if err := m.SetCachePoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetCachePoolable("w") {
		t.Error("after Set(true), GetCachePoolable must be true")
	}
	if err := m.SetCachePoolable(context.Background(), "w", false); err != nil {
		t.Fatal(err)
	}
	if m.GetCachePoolable("w") {
		t.Error("after Set(false), GetCachePoolable must be false")
	}
	// Setting an unregistered workspace errors.
	if err := m.SetCachePoolable(context.Background(), "nope", true); err == nil {
		t.Error("setting cache_poolable on an unregistered workspace should error")
	}
}

func TestRegisterWorkspace_PreservesCachePoolable(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true, CachePoolable: true})
	if !m.GetCachePoolable("w") {
		t.Error("RegisterWorkspace must preserve CachePoolable=true")
	}
}

// Concurrent Set true/false must be race-free (RWMutex); the final settled write
// is authoritative. Run with -race.
func TestSetCachePoolable_Concurrency(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = m.SetCachePoolable(context.Background(), "w", true) }()
		go func() { defer wg.Done(); _ = m.SetCachePoolable(context.Background(), "w", false) }()
		go func() { defer wg.Done(); _ = m.GetCachePoolable("w") }()
	}
	wg.Wait()
	if err := m.SetCachePoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetCachePoolable("w") {
		t.Error("the final settled value must be true")
	}
}
