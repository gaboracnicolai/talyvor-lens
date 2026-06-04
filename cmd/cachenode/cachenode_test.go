package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ─── CacheNodeConfig.Validate ────────────────────

func TestCacheNodeConfigValidate(t *testing.T) {
	if err := (CacheNodeConfig{}).Validate(); err == nil {
		t.Fatal("expected error for empty config")
	}
	good := CacheNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", RedisURL: "redis://x",
		MaxCacheGB: 10, Port: 9091,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected pass with full config, got %v", err)
	}
}

func TestCacheNodeConfigValidate_RejectsBadGB(t *testing.T) {
	cfg := CacheNodeConfig{
		LensURL: "http://l", LensAPIKey: "k", WorkspaceID: "ws",
		NodeURL: "http://node", RedisURL: "redis://x",
		MaxCacheGB: 0, Port: 9091,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for non-positive max_cache_gb")
	}
}

// ─── CacheStorage.Get / Set / Delete ─────────────

func TestCacheStorage_GetMissReturnsEmpty(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	val, owner, err := storage.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "" || owner != "" {
		t.Fatalf("expected ('','',nil), got (%q,%q)", val, owner)
	}
}

func TestCacheStorage_SetStoresWithOwner(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	if err := storage.Set(ctx, "k", "value-bytes", "ws_owner", time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, owner, err := storage.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "value-bytes" || owner != "ws_owner" {
		t.Fatalf("round-trip mismatch: val=%q owner=%q", val, owner)
	}
}

func TestCacheStorage_Delete(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	_ = storage.Set(ctx, "k", "v", "ws", time.Minute)
	if err := storage.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	val, _, _ := storage.Get(ctx, "k")
	if val != "" {
		t.Fatal("expected empty after delete")
	}
}

func TestCacheStorage_Stats_ReportsSize(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_ = storage.Set(ctx, "k"+string(rune('0'+i)), strings.Repeat("x", 1024), "ws", time.Minute)
	}
	stats, err := storage.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalEntries != 10 {
		t.Fatalf("expected 10 entries, got %d", stats.TotalEntries)
	}
	if stats.SizeMB <= 0 {
		t.Fatalf("expected positive size, got %f", stats.SizeMB)
	}
	if stats.MaxSizeMB != 1024 {
		t.Fatalf("expected MaxSizeMB 1024 (1GB), got %f", stats.MaxSizeMB)
	}
}

func TestCacheStorage_HitMissCounters(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	ctx := context.Background()
	_ = storage.Set(ctx, "k", "v", "ws", time.Minute)
	_, _, _ = storage.Get(ctx, "k")     // hit
	_, _, _ = storage.Get(ctx, "miss1") // miss
	_, _, _ = storage.Get(ctx, "miss2") // miss
	stats, _ := storage.Stats(ctx)
	if stats.HitsTotal != 1 || stats.MissTotal != 2 {
		t.Fatalf("expected 1 hit + 2 misses, got %d / %d", stats.HitsTotal, stats.MissTotal)
	}
	if stats.HitRate < 0.33 || stats.HitRate > 0.34 {
		t.Fatalf("expected ~0.333 hit rate, got %f", stats.HitRate)
	}
}

// ─── CacheServer ─────────────────────────────────

func newServer(t *testing.T, secret string) (*httptest.Server, *CacheStorage) {
	storage := NewCacheStorage(newRedis(t), 1)
	srv := httptest.NewServer(NewCacheServer(storage, secret).Handler())
	t.Cleanup(srv.Close)
	return srv, storage
}

func TestCacheServer_GetReturns404OnMiss(t *testing.T) {
	srv, _ := newServer(t, "")
	resp, err := http.Get(srv.URL + "/cache/missing")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCacheServer_PostStoresAndGetRetrieves(t *testing.T) {
	srv, _ := newServer(t, "")
	postBody := `{"value":"hello","ttl_seconds":300,"owner_workspace":"ws_a"}`
	resp, err := http.Post(srv.URL+"/cache/mykey", "application/json", strings.NewReader(postBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/cache/mykey")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body struct {
		Value          string `json:"value"`
		OwnerWorkspace string `json:"owner_workspace"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Value != "hello" || body.OwnerWorkspace != "ws_a" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestCacheServer_RejectsWrongSecret(t *testing.T) {
	srv, _ := newServer(t, "real-secret")
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cache/anything", nil)
	req.Header.Set("X-Node-Secret", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestCacheServer_AcceptsRightSecret(t *testing.T) {
	srv, storage := newServer(t, "real-secret")
	_ = storage.Set(context.Background(), "k", "v", "ws", time.Minute)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/cache/k", nil)
	req.Header.Set("X-Node-Secret", "real-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCacheServer_Health(t *testing.T) {
	srv, _ := newServer(t, "")
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "healthy" {
		t.Fatalf("expected healthy, got %v", out["status"])
	}
}

func TestCacheServer_Stats(t *testing.T) {
	srv, storage := newServer(t, "")
	_ = storage.Set(context.Background(), "k1", strings.Repeat("a", 512), "ws", time.Minute)
	resp, err := http.Get(srv.URL + "/stats")
	if err != nil {
		t.Fatalf("GET /stats: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var out CacheStats
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.TotalEntries != 1 {
		t.Fatalf("expected 1 entry, got %d", out.TotalEntries)
	}
}

// ─── NodeState round-trip ───────────────────────

func TestCacheNodeState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TALYVOR_CACHENODE_STATE", tmp+"/state.json")
	in := CacheNodeState{
		NodeID: "cn1", NodeSecret: "secret", WorkspaceID: "ws",
		LensURL: "http://l", NodeURL: "http://node",
		MaxCacheGB: 10, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveState(in); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if out.NodeID != in.NodeID || out.NodeSecret != in.NodeSecret {
		t.Fatalf("round-trip mismatch: %+v vs %+v", in, out)
	}
	if err := ClearState(); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	out, _ = LoadState()
	if out.NodeID != "" {
		t.Fatal("expected empty state after clear")
	}
}

// ─── TLS config loading ──────────────────────────

func TestCacheNodeConfig_LoadsTLSEnvVars(t *testing.T) {
	t.Setenv("CACHE_NODE_TLS_CERT", "/certs/cache.pem")
	t.Setenv("CACHE_NODE_TLS_KEY", "/certs/cache.key")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "/certs/cache.pem" {
		t.Fatalf("expected TLSCertFile, got %q", cfg.TLSCertFile)
	}
	if cfg.TLSKeyFile != "/certs/cache.key" {
		t.Fatalf("expected TLSKeyFile, got %q", cfg.TLSKeyFile)
	}
}

func TestCacheNodeConfig_TLSDefaultsEmpty(t *testing.T) {
	t.Setenv("CACHE_NODE_TLS_CERT", "")
	t.Setenv("CACHE_NODE_TLS_KEY", "")
	cfg := LoadConfig()
	if cfg.TLSCertFile != "" || cfg.TLSKeyFile != "" {
		t.Fatal("expected empty TLS fields when env vars are absent")
	}
}

// ─── CacheServer TLS ─────────────────────────────

func TestCacheServer_ServesOverTLS(t *testing.T) {
	storage := NewCacheStorage(newRedis(t), 1)
	srv := NewCacheServer(storage, "")
	ts := httptest.NewTLSServer(srv.Handler())
	defer ts.Close()

	client := ts.Client()
	resp, err := client.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("HTTPS GET /health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

