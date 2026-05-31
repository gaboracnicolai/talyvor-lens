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
	return newNodeStore(mock), mock
}

// ─── RecordEmbedHeartbeat ────────────────────────────────────────────────────

func TestRecordEmbedHeartbeat_UpdatesRow(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec("UPDATE embedding_nodes").
		WithArgs("node_1", int64(300)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := store.RecordEmbedHeartbeat(context.Background(), "node_1", 300); err != nil {
		t.Fatalf("RecordEmbedHeartbeat: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordEmbedHeartbeat_NilPool_NoOp(t *testing.T) {
	store := newNodeStore(nil)
	if err := store.RecordEmbedHeartbeat(context.Background(), "x", 0); err != nil {
		t.Fatalf("expected no error with nil pool, got %v", err)
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
	store := newNodeStore(nil)
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
	store := newNodeStore(nil)
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
	now := time.Now().UTC()
	secs := int(StaleThreshold.Seconds())

	// inference_nodes — return no rows (avoids TEXT[] scan complexity in mock)
	mock.ExpectQuery("SELECT id, workspace_id, url, provider, models").
		WithArgs(secs).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "provider", "models",
			"gpu_type", "max_concurrent", "price_per_token", "last_seen_at", "uptime_seconds",
		}))

	// cache_nodes — one live node
	mock.ExpectQuery("SELECT id, workspace_id, url, max_size_gb, last_seen_at").
		WithArgs(secs).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "url", "max_size_gb", "last_seen_at",
		}).AddRow("c1", "ws1", "http://cache1:6379", 10.0, now))

	// embedding_nodes — one live node
	mock.ExpectQuery("SELECT id, workspace_id, url, model, dimensions").
		WithArgs(secs).
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
