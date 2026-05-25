package main

// client.go — Lens client + local state persistence for the
// embedding node. Mirrors cmd/node and cmd/cachenode patterns.

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

// LensClient wraps the embedding-node-specific Lens API surface.
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

// EmbedNodeState persists the registration result so subsequent
// commands (status, etc.) can run without re-registering.
type EmbedNodeState struct {
	NodeID       string    `json:"node_id"`
	NodeSecret   string    `json:"node_secret"`
	WorkspaceID  string    `json:"workspace_id"`
	LensURL      string    `json:"lens_url"`
	NodeURL      string    `json:"node_url"`
	Model        string    `json:"model"`
	Dimensions   int       `json:"dimensions"`
	MaxBatch     int       `json:"max_batch"`
	SpeedTPS     int64     `json:"speed_tps"`
	RegisteredAt time.Time `json:"registered_at"`
}

func statePath() string {
	if v := os.Getenv("TALYVOR_EMBEDNODE_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".talyvor-embednode", "state.json")
}

func SaveState(s EmbedNodeState) error {
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

func LoadState() (EmbedNodeState, error) {
	var s EmbedNodeState
	buf, err := os.ReadFile(statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(buf, &s); err != nil {
		return s, fmt.Errorf("embednode: parse state file: %w", err)
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

// Register hits POST /v1/workspaces/:wsID/embedding-nodes
// (mounted in Batch 2 Item 3). Sends the locally-generated
// secret so Lens stores its hash.
func (c *LensClient) Register(ctx context.Context, cfg EmbedNodeConfig, speedTPS int64) (EmbedNodeState, error) {
	secret := generateSecret()
	payload := map[string]any{
		"url":         cfg.NodeURL,
		"model":       cfg.Model,
		"dimensions":  cfg.Dimensions,
		"max_batch":   cfg.MaxBatch,
		"speed_tps":   speedTPS,
		"node_secret": secret,
	}
	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/embedding-nodes", cfg.WorkspaceID), payload)
	if err != nil {
		return EmbedNodeState{}, err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return EmbedNodeState{}, fmt.Errorf("embednode: decode register response: %w", err)
	}
	if out.ID == "" {
		return EmbedNodeState{}, errors.New("embednode: Lens did not return a node ID")
	}
	state := EmbedNodeState{
		NodeID:       out.ID,
		NodeSecret:   secret,
		WorkspaceID:  cfg.WorkspaceID,
		LensURL:      cfg.LensURL,
		NodeURL:      cfg.NodeURL,
		Model:        cfg.Model,
		Dimensions:   cfg.Dimensions,
		MaxBatch:     cfg.MaxBatch,
		SpeedTPS:     speedTPS,
		RegisteredAt: time.Now().UTC(),
	}
	if err := SaveState(state); err != nil {
		return EmbedNodeState{}, fmt.Errorf("embednode: save state: %w", err)
	}
	return state, nil
}

func (c *LensClient) Deregister(ctx context.Context, state EmbedNodeState) error {
	_, err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/workspaces/%s/embedding-nodes/%s",
			state.WorkspaceID, state.NodeID), nil)
	_ = ClearState()
	return err
}

// Heartbeat is best-effort — Lens currently has no embedding-
// node-specific heartbeat endpoint, so we hit a 404-tolerant
// path. When the heartbeat endpoint lands this stub becomes a
// real call; until then it's a no-op + warning log path.
func (c *LensClient) Heartbeat(ctx context.Context, state EmbedNodeState, speedTPS int64, inflight int64) error {
	// Reuse the cache-node heartbeat shape — same semantics
	// (last_seen_at refresh + counters) and Lens already speaks
	// it. The Lens-side endpoint for embedding nodes is a
	// follow-up; this call will 404 on older deployments which
	// is treated as a non-fatal signal upstream.
	payload := map[string]any{
		"speed_tps": speedTPS,
		"inflight":  inflight,
	}
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/embedding-nodes/%s/heartbeat",
			state.WorkspaceID, state.NodeID), payload)
	return err
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

func generateSecret() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
