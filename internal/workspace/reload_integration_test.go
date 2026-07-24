package workspace

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// ── CONSENT/PRIVACY FAIL-CLOSED after BOUNDED staleness (GREEN) ──
//
// U7c (above) proves the ~30s reload closes the cross-replica staleness window in
// NORMAL operation. These pin the FIX for the failure mode the chaos recon surfaced:
// when a replica's reload keeps FAILING (a DB outage at THAT replica), build-then-swap
// keeps the OLD cache — so without a bound a revoked consent / privacy opt-out would
// stay UNHONORED forever. Past LENS_WORKSPACE_MAX_STALENESS the consent/privacy
// accessors fail CLOSED to their conservative value; WITHIN the bound the normal grace
// is UN-degraded; a successful reload reverts to the true value. The override is
// MORE-CONSERVATIVE-ONLY (effective ≤ cached) — it can pause pooling / clamp logging,
// never the reverse. An injectable clock drives staleness deterministically (no sleeps).

const fcBound = 50 * time.Millisecond // the staleness bound for these tests

// testClock is a controllable monotonic clock injected as Manager.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock               { return &testClock{t: time.Unix(1_700_000_000, 0).UTC()} }
func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newFailClosedB builds replica B over the pool with the injectable clock + the
// fail-closed bound, then loads once so lastReload = the clock's base.
func newFailClosedB(t *testing.T, pool *pgxpool.Pool) (*Manager, *testClock) {
	t.Helper()
	clk := newTestClock()
	m := New(pool)
	m.now = clk.now
	m.SetMaxStaleness(fcBound)
	if err := m.LoadAll(context.Background()); err != nil {
		t.Fatalf("B LoadAll: %v", err)
	}
	return m, clk
}

// failReload runs one reload that ERRORS (canceled ctx) — the DB outage at B. A failed
// reload must NOT refresh lastReload, so staleness keeps accruing across the outage.
func failReload(t *testing.T, m *Manager) {
	t.Helper()
	down, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Reload(down); err == nil {
		t.Fatal("setup: reload must fail under the simulated outage")
	}
}

// TestFailClosed_CachePoolable — cross-tenant pooling consent pauses past the bound.
func TestFailClosed_CachePoolable(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA := New(pool)
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-pool", Name: "w", CachePoolable: true})

	mB, clk := newFailClosedB(t, pool)
	if !mB.GetCachePoolable("ws-fc-pool") {
		t.Fatal("fresh: B must serve poolable=true")
	}
	// Within bound (one missed tick) — grace un-degraded: stale value still served.
	clk.advance(fcBound - 10*time.Millisecond)
	failReload(t, mB)
	if !mB.GetCachePoolable("ws-fc-pool") {
		t.Error("within bound: normal grace must be un-degraded (poolable still true)")
	}
	// Beyond bound — FAIL CLOSED: pooling paused (consent unconfirmable).
	clk.advance(20 * time.Millisecond)
	failReload(t, mB)
	if mB.GetCachePoolable("ws-fc-pool") {
		t.Error("beyond bound: must FAIL CLOSED — poolable=false when consent can't be confirmed")
	}
	// Recovery — a successful reload reverts to the true value.
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if !mB.GetCachePoolable("ws-fc-pool") {
		t.Error("after a successful reload, B must revert to poolable=true")
	}
}

// TestFailClosed_DistillPoolable — cross-tenant distill sharing consent, same shape.
func TestFailClosed_DistillPoolable(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA := New(pool)
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-dpool", Name: "w", DistillPoolable: true})

	mB, clk := newFailClosedB(t, pool)
	if !mB.GetDistillPoolable("ws-fc-dpool") {
		t.Fatal("fresh: B must serve distill_poolable=true")
	}
	clk.advance(fcBound - 10*time.Millisecond)
	failReload(t, mB)
	if !mB.GetDistillPoolable("ws-fc-dpool") {
		t.Error("within bound: grace un-degraded (distill_poolable still true)")
	}
	clk.advance(20 * time.Millisecond)
	failReload(t, mB)
	if mB.GetDistillPoolable("ws-fc-dpool") {
		t.Error("beyond bound: must FAIL CLOSED — distill_poolable=false")
	}
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if !mB.GetDistillPoolable("ws-fc-dpool") {
		t.Error("after reload, B must revert to distill_poolable=true")
	}
}

// TestFailClosed_DistillPolicy — distillation stops past the bound (floor = disabled).
func TestFailClosed_DistillPolicy(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA := New(pool)
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-dp", Name: "w", DistillPolicy: DistillAlways})

	mB, clk := newFailClosedB(t, pool)
	if mB.GetDistillPolicy("ws-fc-dp") != DistillAlways {
		t.Fatal("fresh: B must serve distill_policy=always")
	}
	clk.advance(fcBound - 10*time.Millisecond)
	failReload(t, mB)
	if mB.GetDistillPolicy("ws-fc-dp") != DistillAlways {
		t.Error("within bound: grace un-degraded (distill_policy still always)")
	}
	clk.advance(20 * time.Millisecond)
	failReload(t, mB)
	if got := mB.GetDistillPolicy("ws-fc-dp"); got != DistillDisabled {
		t.Errorf("beyond bound: must FAIL CLOSED to disabled, got %q", got)
	}
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if mB.GetDistillPolicy("ws-fc-dp") != DistillAlways {
		t.Error("after reload, B must revert to distill_policy=always")
	}
}

// TestFailClosed_Logging_Surgical — logging has no single conservative direction, so
// the clamp is SURGICAL: a stale value MORE PERMISSIVE than the default (full) clamps
// to metadata (reverting an unconfirmed relaxation — the worst case is a revoked full
// still capturing raw prompt_text), but is NEVER forced to none; a metadata/none value
// is untouched (forcing none would under-serve a compliance/metadata tenant).
func TestFailClosed_Logging_Surgical(t *testing.T) {
	pool := restartTestPool(t)
	ctx := context.Background()
	mA := New(pool)
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-full", Name: "w", LoggingPolicy: LoggingFull})
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-meta", Name: "w", LoggingPolicy: LoggingMetadata})

	mB, clk := newFailClosedB(t, pool)
	if mB.GetLoggingPolicy("ws-fc-full") != LoggingFull {
		t.Fatal("fresh: full workspace must serve full")
	}
	// Within bound — grace un-degraded: full still served.
	clk.advance(fcBound - 10*time.Millisecond)
	failReload(t, mB)
	if mB.GetLoggingPolicy("ws-fc-full") != LoggingFull {
		t.Error("within bound: grace un-degraded (full still served)")
	}
	// Beyond bound — surgical clamp: full → metadata, NOT none.
	clk.advance(20 * time.Millisecond)
	failReload(t, mB)
	if got := mB.GetLoggingPolicy("ws-fc-full"); got != LoggingMetadata {
		t.Errorf("surgical clamp: stale full must clamp to metadata, got %q", got)
	}
	if mB.GetLoggingPolicy("ws-fc-full") == LoggingNone {
		t.Error("surgical: stale full must NOT be forced to none (compliance protection)")
	}
	// A metadata workspace (== default) is NOT clamped and NOT forced to none.
	if got := mB.GetLoggingPolicy("ws-fc-meta"); got != LoggingMetadata {
		t.Errorf("surgical: stale metadata must be unchanged (not forced to none), got %q", got)
	}
	// Recovery reverts the clamp.
	if err := mB.Reload(ctx); err != nil {
		t.Fatalf("recovery reload: %v", err)
	}
	if mB.GetLoggingPolicy("ws-fc-full") != LoggingFull {
		t.Error("after reload, full must revert to full")
	}
}

// TestFailClosed_NeverMorePermissive — the MORE-CONSERVATIVE-ONLY invariant: a
// workspace already at its floor (poolable=false, distill disabled, logging=none) is
// unchanged by staleness — the override only ever LOWERS permissiveness, never raises it.
func TestFailClosed_NeverMorePermissive(t *testing.T) {
	pool := restartTestPool(t)
	mA := New(pool)
	mustRegisterActive(t, mA, Workspace{ID: "ws-fc-floor", Name: "w",
		CachePoolable: false, DistillPoolable: false,
		LoggingPolicy: LoggingNone, DistillPolicy: DistillDisabled})
	// Registration now defaults cache_poolable=true; the "floor" is an OPTED-OUT
	// workspace, so establish poolable=false via the consent setter (written to the
	// DB before B loads it) — otherwise B would legitimately load poolable=true.
	if err := mA.SetCachePoolable(context.Background(), "ws-fc-floor", false); err != nil {
		t.Fatalf("pin floor cache_poolable=false: %v", err)
	}

	mB, clk := newFailClosedB(t, pool)
	clk.advance(fcBound + 50*time.Millisecond) // stale beyond bound
	failReload(t, mB)
	if mB.GetCachePoolable("ws-fc-floor") {
		t.Error("invariant: stale must not RAISE cache_poolable to true")
	}
	if mB.GetDistillPoolable("ws-fc-floor") {
		t.Error("invariant: stale must not RAISE distill_poolable to true")
	}
	if got := mB.GetLoggingPolicy("ws-fc-floor"); got != LoggingNone {
		t.Errorf("invariant: stale must not raise logging above none, got %q", got)
	}
	if got := mB.GetDistillPolicy("ws-fc-floor"); got != DistillDisabled {
		t.Errorf("invariant: stale must not raise distill_policy above disabled, got %q", got)
	}
}

// TestFailClosed_ZeroLastReload_NeverFailsClosed — BOOTSTRAP guard: before the first
// SUCCESSFUL reload lastReload is zero, and the override is INERT even with a real
// pool, the bound set, and the clock far past it (otherwise boot fail-closes the whole
// cache). Real-PG: isolates the zero-lastReload guard (pool!=nil, bound>0, clock past).
func TestFailClosed_ZeroLastReload_NeverFailsClosed(t *testing.T) {
	pool := restartTestPool(t)
	clk := newTestClock()
	mB := New(pool)
	mB.now = clk.now
	mB.SetMaxStaleness(fcBound)
	// Register populates B's map but does NOT stamp lastReload (only a successful
	// LoadAll does), so B has "never loaded".
	mustRegisterActive(t, mB, Workspace{ID: "ws-boot", Name: "w", CachePoolable: true, LoggingPolicy: LoggingFull})
	if !mB.lastReload.IsZero() {
		t.Fatal("setup: lastReload must be zero before any successful LoadAll")
	}
	clk.advance(fcBound * 100) // far past the bound
	if !mB.GetCachePoolable("ws-boot") {
		t.Error("bootstrap (zero lastReload): must NOT fail closed — serve poolable=true")
	}
	if got := mB.GetLoggingPolicy("ws-boot"); got != LoggingFull {
		t.Errorf("bootstrap (zero lastReload): must NOT clamp logging, got %q want full", got)
	}
}

// TestFailClosed_NilPool_NeverFailsClosed — BOOTSTRAP guard: an in-memory Manager
// (pool==nil, no reload concept) is fully inert to the override. In-memory; runs
// WITHOUT LENS_TEST_DATABASE_URL.
func TestFailClosed_NilPool_NeverFailsClosed(t *testing.T) {
	clk := newTestClock()
	m := New(nil)
	m.now = clk.now
	m.SetMaxStaleness(fcBound)
	mustRegisterActive(t, m, Workspace{ID: "ws-mem", Name: "w", CachePoolable: true, LoggingPolicy: LoggingFull})
	clk.advance(fcBound * 100)
	if !m.GetCachePoolable("ws-mem") {
		t.Error("nil pool: must NOT fail closed — serve poolable=true")
	}
	if got := m.GetLoggingPolicy("ws-mem"); got != LoggingFull {
		t.Errorf("nil pool: must NOT clamp logging, got %q want full", got)
	}
}
