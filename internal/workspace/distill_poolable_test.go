package workspace

import (
	"context"
	"sync"
	"testing"
)

// Private-by-default: an unregistered or unset workspace is never distill-poolable,
// so cross-tenant distill sharing can never turn on implicitly.
func TestDistillPoolable_DefaultFalse(t *testing.T) {
	m := New(nil)
	if m.GetDistillPoolable("unknown") {
		t.Error("an unregistered workspace must be non-poolable (private by default)")
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if m.GetDistillPoolable("w") {
		t.Error("a workspace with no flag set must default to non-poolable")
	}
}

func TestSetDistillPoolable_InMemory(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	if err := m.SetDistillPoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetDistillPoolable("w") {
		t.Error("after Set(true), GetDistillPoolable must be true")
	}
	if err := m.SetDistillPoolable(context.Background(), "w", false); err != nil {
		t.Fatal(err)
	}
	if m.GetDistillPoolable("w") {
		t.Error("after Set(false), GetDistillPoolable must be false")
	}
	// Setting an unregistered workspace errors.
	if err := m.SetDistillPoolable(context.Background(), "nope", true); err == nil {
		t.Error("setting distill_poolable on an unregistered workspace should error")
	}
}

// distill_poolable is a SEPARATE consent from cache_poolable — setting one must
// never move the other.
func TestDistillPoolable_IndependentOfCachePoolable(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if err := m.SetDistillPoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if m.GetCachePoolable("w") {
		t.Error("setting distill_poolable must NOT enable cache_poolable")
	}
	if err := m.SetCachePoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetDistillPoolable("w") {
		t.Error("setting cache_poolable must NOT disturb distill_poolable")
	}
}

func TestRegisterWorkspace_PreservesDistillPoolable(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true, DistillPoolable: true})
	if !m.GetDistillPoolable("w") {
		t.Error("RegisterWorkspace must preserve DistillPoolable=true")
	}
}

// Concurrent Set true/false must be race-free (RWMutex). Run with -race.
func TestSetDistillPoolable_Concurrency(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = m.SetDistillPoolable(context.Background(), "w", true) }()
		go func() { defer wg.Done(); _ = m.SetDistillPoolable(context.Background(), "w", false) }()
		go func() { defer wg.Done(); _ = m.GetDistillPoolable("w") }()
	}
	wg.Wait()
	if err := m.SetDistillPoolable(context.Background(), "w", true); err != nil {
		t.Fatal(err)
	}
	if !m.GetDistillPoolable("w") {
		t.Error("the final settled value must be true")
	}
}
