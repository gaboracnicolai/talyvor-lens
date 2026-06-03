package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockStore(t *testing.T) (*NodeStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	return newNodeStore(mock, nil), mock
}

// ─── RecordEmbedHeartbeat ────────────────────────────────────────────────────

func TestRecordEmbedHeartbeat_UpdatesRow(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec("UPDATE embedding_nodes").
		WithArgs("node_1", int64(300), "ws-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	found, err := store.RecordEmbedHeartbeat(context.Background(), "node_1", "ws-1", 300)
	if err != nil {
		t.Fatalf("RecordEmbedHeartbeat: %v", err)
	}
	if !found {
		t.Fatal("expected found=true when UPDATE affects 1 row")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordEmbedHeartbeat_NilPool_NoOp(t *testing.T) {
	store := newNodeStore(nil, nil)
	found, err := store.RecordEmbedHeartbeat(context.Background(), "x", "ws", 0)
	if err != nil {
		t.Fatalf("expected no error with nil pool, got %v", err)
	}
	if found {
		t.Fatal("expected found=false with nil pool")
	}
}

// ─── MarkStaleInactive ───────────────────────────────────────────────────────

func TestMarkStaleInactive_UpdatesAllThreeTables(t *testing.T) {
	store, mock := newMockStore(t)
	secs := int(StaleThreshold.Seconds())
	for _, table := range []string{"inference_nodes", "cache_nodes", "embedding_nodes"} {
		mock.ExpectExec("UPDATE "+table).
			WithArgs(secs).
			WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	}
	n, err := store.MarkStaleInactive(context.Background(), StaleThreshold)
	if err != nil {
		t.Fatalf("MarkStaleInactive: %v", err)
	}
	// 2 rows per table × 3 tables = 6
	if n != 6 {
		t.Fatalf("expected 6 rows deactivated, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestMarkStaleInactive_NilPool_ReturnsZero(t *testing.T) {
	store := newNodeStore(nil, nil)
	n, err := store.MarkStaleInactive(context.Background(), StaleThreshold)
	if err != nil || n != 0 {
		t.Fatalf("expected (0, nil), got (%d, %v)", n, err)
	}
}

func TestMarkStaleInactive_ZeroRowsWhenAllAlive(t *testing.T) {
	store, mock := newMockStore(t)
	secs := int(StaleThreshold.Seconds())
	for _, table := range []string{"inference_nodes", "cache_nodes", "embedding_nodes"} {
		mock.ExpectExec("UPDATE "+table).
			WithArgs(secs).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	}
	n, err := store.MarkStaleInactive(context.Background(), StaleThreshold)
	if err != nil {
		t.Fatalf("MarkStaleInactive: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows deactivated, got %d", n)
	}
}

// ─── Snapshot ────────────────────────────────────────────────────────────────

func TestSnapshot_NilPool_ReturnsEmptySnapshot(t *testing.T) {
	store := newNodeStore(nil, nil)
	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if len(snap.InferenceNodes) != 0 || len(snap.CacheNodes) != 0 || len(snap.EmbeddingNodes) != 0 {
		t.Fatalf("expected empty snapshot with nil pool, got %+v", snap)
	}
	if snap.GeneratedAt.IsZero() {
		t.Fatal("GeneratedAt should not be zero")
	}
}

func TestSnapshot_ReturnsCacheAndEmbeddingNodes(t *testing.T) {
	store, mock := newMockStore(t)
	// Timestamps must be within StaleThreshold so the Go-side isLive()
	// fallback (Postgres last_seen_at) includes them. hb is nil here so
	// the fallback is always used.
	now := time.Now().UTC()

	// inference_nodes — return no rows (avoids TEXT[] scan complexity in mock)
	// SQL no longer passes $1 (time filter moved to Go).
	mock.ExpectQuery("SELECT id, workspace_id, url, provider, models").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models",
			"gpu_type", "max_concurrent", "price_per_token", "last_seen_at", "uptime_seconds",
		}))

	// cache_nodes — one live node
	mock.ExpectQuery("SELECT id, workspace_id, url, max_size_gb, last_seen_at").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "max_size_gb", "last_seen_at",
		}).AddRow("c1", "ws1", "http://cache1:6379", 10.0, now))

	// embedding_nodes — one live node
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "last_seen_at", "uptime_seconds",
		}).AddRow("e1", "ws1", "http://embed1:9092", "text-embedding-3-small",
			1536, 100, 500, now, int64(7200)))

	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.InferenceNodes) != 0 {
		t.Fatalf("expected 0 inference nodes, got %d", len(snap.InferenceNodes))
	}
	if len(snap.CacheNodes) != 1 || snap.CacheNodes[0].ID != "c1" {
		t.Fatalf("unexpected cache nodes: %+v", snap.CacheNodes)
	}
	if len(snap.EmbeddingNodes) != 1 || snap.EmbeddingNodes[0].ID != "e1" {
		t.Fatalf("unexpected embedding nodes: %+v", snap.EmbeddingNodes)
	}
	if snap.EmbeddingNodes[0].Model != "text-embedding-3-small" {
		t.Fatalf("unexpected model: %s", snap.EmbeddingNodes[0].Model)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── Snapshot HA behaviour (Redis heartbeat liveness) ────────────────────────

// TestSnapshot_HA_PrefersRedisHeartbeat verifies that a node whose Postgres
// last_seen_at is stale (> StaleThreshold ago) is still included in the
// snapshot when a fresh Redis heartbeat exists. This is the core HA property:
// a heartbeat recorded by any Lens instance keeps the node live in the fleet
// view even before the reconciler flushes a new Postgres timestamp.
func TestSnapshot_HA_PrefersRedisHeartbeat(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	store := newNodeStore(mock, hb)

	// c-fresh-redis has a stale Postgres timestamp but a live Redis heartbeat.
	staleTime := time.Now().Add(-(StaleThreshold + time.Minute))
	if err := hb.Record(context.Background(), "cache", "c-fresh-redis", 0); err != nil {
		t.Fatalf("Record: %v", err)
	}

	mock.ExpectQuery("SELECT id, workspace_id, url, provider, models").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models",
			"gpu_type", "max_concurrent", "price_per_token", "last_seen_at", "uptime_seconds",
		}))
	mock.ExpectQuery("SELECT id, workspace_id, url, max_size_gb, last_seen_at").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "max_size_gb", "last_seen_at",
		}).AddRow("c-fresh-redis", "ws1", "http://cache1:6379", 10.0, staleTime))
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "last_seen_at", "uptime_seconds",
		}))

	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.CacheNodes) != 1 || snap.CacheNodes[0].ID != "c-fresh-redis" {
		t.Fatalf("expected c-fresh-redis included via Redis heartbeat, got %+v", snap.CacheNodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("HA prefer-redis expectations: %v", err)
	}
}

// TestSnapshot_HA_FallsBackToPostgresLastSeen verifies that a node with a
// fresh Postgres last_seen_at but NO Redis heartbeat is still included via
// the Postgres fallback path. Single-instance / no-Redis deployments must
// behave identically to the pre-HA code.
func TestSnapshot_HA_FallsBackToPostgresLastSeen(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc) // Redis present but no heartbeat recorded

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	store := newNodeStore(mock, hb)

	now := time.Now().UTC()

	mock.ExpectQuery("SELECT id, workspace_id, url, provider, models").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models",
			"gpu_type", "max_concurrent", "price_per_token", "last_seen_at", "uptime_seconds",
		}))
	mock.ExpectQuery("SELECT id, workspace_id, url, max_size_gb, last_seen_at").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "max_size_gb", "last_seen_at",
		}).AddRow("c-pg-only", "ws1", "http://cache2:6379", 5.0, now))
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "last_seen_at", "uptime_seconds",
		}))

	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.CacheNodes) != 1 || snap.CacheNodes[0].ID != "c-pg-only" {
		t.Fatalf("expected c-pg-only included via Postgres fallback, got %+v", snap.CacheNodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("HA fallback expectations: %v", err)
	}
}

// TestSnapshot_HA_ExcludesIfBothStale verifies that a node stale in both Redis
// and Postgres is excluded from the snapshot even when a HeartbeatStore is
// configured. Both liveness signals must agree the node is dead.
func TestSnapshot_HA_ExcludesIfBothStale(t *testing.T) {
	rc, _ := newTestRedis(t)
	hb := newHeartbeatStore(rc) // Redis present but no heartbeat recorded

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	store := newNodeStore(mock, hb)

	staleTime := time.Now().Add(-(StaleThreshold + time.Minute))

	mock.ExpectQuery("SELECT id, workspace_id, url, provider, models").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models",
			"gpu_type", "max_concurrent", "price_per_token", "last_seen_at", "uptime_seconds",
		}))
	mock.ExpectQuery("SELECT id, workspace_id, url, max_size_gb, last_seen_at").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "max_size_gb", "last_seen_at",
		}).AddRow("c-dead", "ws1", "http://cache3:6379", 5.0, staleTime))
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "model", "dimensions",
			"max_batch", "speed_tps", "last_seen_at", "uptime_seconds",
		}))

	snap, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.CacheNodes) != 0 {
		t.Fatalf("expected dead node excluded, got %+v", snap.CacheNodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("HA both-stale expectations: %v", err)
	}
}
