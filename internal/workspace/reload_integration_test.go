package workspace

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// U7c — cross-replica propagation proofs. Two Manager instances over ONE shared
// pool stand in for two replicas: instance A persists a change (to the DB), B
// still serves the stale cached value, then B.Reload(ctx) makes it visible. This
// pins that the reload (the StartRefresh ticker drives the same Reload) actually
// closes the staleness window.

func mustRegisterActive(t *testing.T, m *Manager, ws Workspace) {
	t.Helper()
	ws.Active = true
	ws.AllowedModels = []string{}
	ws.AllowedProviders = []string{}
	if err := m.RegisterWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("RegisterWorkspace %s: %v", ws.ID, err)
	}
}

// TestReload_PropagatesLoggingPolicy_AcrossInstances — a logging-policy tightening
// on A is stale on B until B reloads.
func TestReload_PropagatesLoggingPolicy_AcrossInstances(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA, mB := New(pool), New(pool)

	mustRegisterActive(t, mA, Workspace{ID: "ws-log", Name: "w", LoggingPolicy: LoggingMetadata})
	if err := mB.LoadAll(ctx); err != nil {
		t.Fatalf("B LoadAll: %v", err)
	}
	if got := mB.GetLoggingPolicy("ws-log"); got != LoggingMetadata {
		t.Fatalf("B initial = %q, want metadata", got)
	}

	// A tightens to LoggingNone (persists to DB + A's map).
	if err := mA.SetLoggingPolicy(ctx, "ws-log", LoggingNone); err != nil {
		t.Fatalf("A SetLoggingPolicy: %v", err)
	}
	// Staleness precondition: B still serves the OLD value before it reloads.
	if got := mB.GetLoggingPolicy("ws-log"); got != LoggingMetadata {
		t.Errorf("staleness precondition: B should still serve OLD metadata before reload, got %q", got)
	}
	// After reload, B serves the NEW value.
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("B Reload: %v", err)
	}
	if got := mB.GetLoggingPolicy("ws-log"); got != LoggingNone {
		t.Errorf("after Reload, B must serve NEW none, got %q", got)
	}
}

// TestReload_PropagatesCachePoolable_AcrossInstances — the PRIVACY flag: a
// cache-pooling opt-OUT on A is stale on B (B could keep pooling that tenant's
// cache) until B reloads.
func TestReload_PropagatesCachePoolable_AcrossInstances(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA, mB := New(pool), New(pool)

	mustRegisterActive(t, mA, Workspace{ID: "ws-pool", Name: "w", CachePoolable: true})
	if err := mB.LoadAll(ctx); err != nil {
		t.Fatalf("B LoadAll: %v", err)
	}
	if !mB.GetCachePoolable("ws-pool") {
		t.Fatal("B initial cache_poolable = false, want true")
	}

	// A revokes pooling (privacy opt-out) — persists.
	if err := mA.SetCachePoolable(ctx, "ws-pool", false); err != nil {
		t.Fatalf("A SetCachePoolable: %v", err)
	}
	// Staleness precondition: B still reports poolable=true (the privacy gap).
	if !mB.GetCachePoolable("ws-pool") {
		t.Error("staleness precondition: B should still report poolable=true before reload")
	}
	// After reload, B honors the opt-out.
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("B Reload: %v", err)
	}
	if mB.GetCachePoolable("ws-pool") {
		t.Error("after Reload, B must honor the cache-pooling opt-out (privacy) — still poolable")
	}
}

// TestReload_PropagatesDeactivation_AcrossInstances — the case the OLD in-place
// scan could not handle: a deactivated workspace must be DROPPED on reload, not
// linger with a stale policy. RED against the pre-refactor in-place LoadAll;
// GREEN with build-then-swap.
func TestReload_PropagatesDeactivation_AcrossInstances(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA, mB := New(pool), New(pool)

	mustRegisterActive(t, mA, Workspace{ID: "ws-del", Name: "w"})
	if err := mB.LoadAll(ctx); err != nil {
		t.Fatalf("B LoadAll: %v", err)
	}
	if _, ok := mB.GetWorkspace("ws-del"); !ok {
		t.Fatal("precondition: B must have ws-del before deactivation")
	}

	// Deactivate in the DB (loadAllSQL filters active=true → it leaves the result set).
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET active=false WHERE id=$1`, "ws-del"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("B Reload: %v", err)
	}
	if _, ok := mB.GetWorkspace("ws-del"); ok {
		t.Error("DEACTIVATION not propagated: ws-del still in B's map after Reload — build-then-swap must drop it (the in-place-scan bug)")
	}
}

// TestReload_ConcurrentReaderVsReload_NoRace — under -race: readers racing a
// reload loop see either the whole old or whole new map, never a torn one.
func TestReload_ConcurrentReaderVsReload_NoRace(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	m := New(pool)
	mustRegisterActive(t, m, Workspace{ID: "ws-race", Name: "w", CachePoolable: true, LoggingPolicy: LoggingMetadata})
	if err := m.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.GetCachePoolable("ws-race")
				_ = m.GetLoggingPolicy("ws-race")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.Reload(ctx)
		}
	}()
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestStartRefresh_CleanExitOnCancel_NoLeak — the ticker goroutine exits on ctx
// cancel (no goroutine leak).
func TestStartRefresh_CleanExitOnCancel_NoLeak(t *testing.T) {
	pool := restartTestPool(t)
	m := New(pool)
	runtime.GC()
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	m.StartRefresh(ctx, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond) // let it tick a few times
	cancel()

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("StartRefresh goroutine leaked after ctx cancel: base=%d now=%d", base, n)
	}
}
