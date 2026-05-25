package main

// client.go — Lens client + local state persistence for the
// cache node. Mirrors cmd/node/client.go but talks to the
// /v1/workspaces/:wsID/cache-nodes endpoints.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// LensClient wraps the Lens REST API surface a cache node needs.
type LensClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewLensClient(baseURL, apiKey string) *LensClient {
	return &LensClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// CacheNodeState is the persisted shape — what the cache node
// writes to ~/.talyvor-cachenode/state.json after registration.
type CacheNodeState struct {
	NodeID       string    `json:"node_id"`
	NodeSecret   string    `json:"node_secret"`
	WorkspaceID  string    `json:"workspace_id"`
	LensURL      string    `json:"lens_url"`
	NodeURL      string    `json:"node_url"`
	MaxCacheGB   float64   `json:"max_cache_gb"`
	RegisteredAt time.Time `json:"registered_at"`
}

func statePath() string {
	if v := os.Getenv("TALYVOR_CACHENODE_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".talyvor-cachenode", "state.json")
}

// SaveState writes the state file with 0600 perms (the node
// secret is sensitive).
func SaveState(s CacheNodeState) error {
	path := statePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o600)
}

// LoadState returns (zero, nil) when no state file exists.
func LoadState() (CacheNodeState, error) {
	var s CacheNodeState
	buf, err := os.ReadFile(statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(buf, &s); err != nil {
		return s, fmt.Errorf("cachenode: parse state file: %w", err)
	}
	return s, nil
}

func ClearState() error {
	err := os.Remove(statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ─── Lens API calls ──────────────────────────────

// Register tells Lens about the node + persists the returned
// state. Generates the node secret locally so the operator can
// see it once (it lands in state.json).
func (c *LensClient) Register(ctx context.Context, cfg CacheNodeConfig) (CacheNodeState, error) {
	secret := generateSecret()
	payload := map[string]any{
		"url":          cfg.NodeURL,
		"max_size_gb":  cfg.MaxCacheGB,
		"node_secret":  secret,
		"share":        cfg.ShareEnabled,
	}
	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/cache-nodes", cfg.WorkspaceID), payload)
	if err != nil {
		return CacheNodeState{}, err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return CacheNodeState{}, fmt.Errorf("cachenode: decode register response: %w", err)
	}
	if out.ID == "" {
		return CacheNodeState{}, errors.New("cachenode: Lens did not return a node ID")
	}
	state := CacheNodeState{
		NodeID:       out.ID,
		NodeSecret:   secret,
		WorkspaceID:  cfg.WorkspaceID,
		LensURL:      cfg.LensURL,
		NodeURL:      cfg.NodeURL,
		MaxCacheGB:   cfg.MaxCacheGB,
		RegisteredAt: time.Now().UTC(),
	}
	if err := SaveState(state); err != nil {
		return CacheNodeState{}, fmt.Errorf("cachenode: save state: %w", err)
	}
	return state, nil
}

// Deregister flips the node inactive on Lens + clears local
// state. Local clear happens unconditionally so a re-register
// after a failed deregister isn't booby-trapped.
func (c *LensClient) Deregister(ctx context.Context, state CacheNodeState) error {
	_, err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/workspaces/%s/cache-nodes/%s",
			state.WorkspaceID, state.NodeID), nil)
	_ = ClearState()
	return err
}

// Heartbeat pings Lens with the local stats. Errors are non-
// fatal — Lens marks unhealthy after 90s of silence.
func (c *LensClient) Heartbeat(ctx context.Context, state CacheNodeState, entries int, sizeMB, hitRate float64) error {
	payload := map[string]any{
		"entries":  entries,
		"size_mb":  sizeMB,
		"hit_rate": hitRate,
	}
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/cache-nodes/%s/heartbeat",
			state.WorkspaceID, state.NodeID), payload)
	return err
}

// Earnings is currently rolled up via the existing cache-mining
// endpoint on Lens (one workspace, one mining track per call).
// Cache-node-specific earnings will land in a follow-up.
func (c *LensClient) Earnings(ctx context.Context, workspaceID string) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/v1/workspaces/%s/tokens/mining/cache", workspaceID), nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("cachenode: decode earnings: %w", err)
	}
	return out, nil
}

// ─── HTTP plumbing ───────────────────────────────

func (c *LensClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lens: %s %s → %d: %s", method, path, resp.StatusCode, buf)
	}
	return buf, nil
}

// generateSecret returns 32 random hex chars — used for the
// node-to-Lens shared secret.
func generateSecret() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
