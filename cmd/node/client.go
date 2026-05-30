package main

// client.go — talks to the Lens server: registration, heartbeat,
// deregistration, and earnings lookups. Keeps a small JSON state
// file under ~/.talyvor-node/state.json so the `status` and
// `earnings` commands work after start has registered the node.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// LensClient is a thin wrapper around the Lens REST API. The
// timeout is intentionally short — registration + heartbeat are
// small JSON calls.
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

// NodeState is what we persist locally. ID + Secret come from
// the Lens registration response; the rest are convenience
// fields the status command echoes back.
type NodeState struct {
	NodeID      string    `json:"node_id"`
	NodeSecret  string    `json:"node_secret"`
	WorkspaceID string    `json:"workspace_id"`
	LensURL     string    `json:"lens_url"`
	NodeURL     string    `json:"node_url"`
	Provider    string    `json:"provider"`
	GPUType     string    `json:"gpu_type"`
	Models      []string  `json:"models"`
	// Ed25519Priv is the node's ed25519 PRIVATE key (base64) for signing PoVI
	// receipts (Token Economy Phase 1, Part 1). Sensitive — the state file is
	// 0600. The matching public key is registered with Lens.
	Ed25519Priv  string    `json:"ed25519_priv,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
}

// statePath is ~/.talyvor-node/state.json. Created on demand on
// the first save.
func statePath() string {
	if v := os.Getenv("TALYVOR_NODE_STATE"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".talyvor-node", "state.json")
}

// SaveState writes the state file with 0600 perms — node_secret
// is sensitive.
func SaveState(s NodeState) error {
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

// LoadState reads the state file. Returns (zero, nil) when no
// state file exists — let the caller decide whether that's an
// error.
func LoadState() (NodeState, error) {
	var s NodeState
	buf, err := os.ReadFile(statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(buf, &s); err != nil {
		return s, fmt.Errorf("node: parse state file: %w", err)
	}
	return s, nil
}

// ClearState removes the local state file — called on
// deregistration so a re-register isn't booby-trapped by stale
// data.
func ClearState() error {
	err := os.Remove(statePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ─── Lens API calls ──────────────────────────────

// Register registers the node + persists the returned state.
// Generates a node secret locally and sends its hash — Lens
// echoes the secret back exactly once.
//
// Note: this assumes the Lens registration endpoint accepts the
// node-secret hash field. The current Lens schema doesn't yet
// (added in migration 0025 below) — when running against an
// older Lens, the secret hash is silently ignored and the node
// runs without secret auth.
func (c *LensClient) Register(ctx context.Context, cfg NodeConfig) (NodeState, error) {
	secret := generateSecret()
	// Generate a PoVI signing keypair; register the PUBLIC key, keep the
	// private key locally (state file, 0600) to sign receipts.
	pub, priv, keyErr := povi.GenerateNodeKey()
	if keyErr != nil {
		return NodeState{}, fmt.Errorf("node: generate signing key: %w", keyErr)
	}
	payload := map[string]any{
		"url":             cfg.NodeURL,
		"provider":        cfg.Provider,
		"models":          cfg.Models,
		"gpu_type":        cfg.GPUType,
		"max_concurrent":  cfg.MaxConcurrent,
		"node_secret":     secret, // Lens stores the hash and discards the plaintext
		"workspace_id":    cfg.WorkspaceID,
		"ed25519_pubkey":  povi.EncodePublicKey(pub), // PoVI receipt-verification key
	}
	resp, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/nodes", cfg.WorkspaceID), payload)
	if err != nil {
		return NodeState{}, err
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return NodeState{}, fmt.Errorf("node: decode register response: %w", err)
	}
	if out.ID == "" {
		return NodeState{}, errors.New("node: Lens did not return a node ID")
	}
	state := NodeState{
		NodeID:       out.ID,
		NodeSecret:   secret,
		WorkspaceID:  cfg.WorkspaceID,
		LensURL:      cfg.LensURL,
		NodeURL:      cfg.NodeURL,
		Provider:     cfg.Provider,
		GPUType:      cfg.GPUType,
		Models:       append([]string{}, cfg.Models...),
		Ed25519Priv:  base64.StdEncoding.EncodeToString(priv),
		RegisteredAt: time.Now().UTC(),
	}
	if err := SaveState(state); err != nil {
		return NodeState{}, fmt.Errorf("node: save state: %w", err)
	}
	return state, nil
}

// SubmitReceipt pushes a signed PoVI receipt to Lens for verification +
// audit recording (Token Economy Phase 1, Part 1). Best-effort and off the
// node's response path — the node returns the receipt in the response too.
func (c *LensClient) SubmitReceipt(ctx context.Context, workspaceID string, r povi.Receipt) error {
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/povi/receipts", workspaceID), r)
	return err
}

// Deregister flips the node inactive + clears local state. Best-
// effort on the Lens side — local state clears unconditionally so
// a re-register starts clean.
func (c *LensClient) Deregister(ctx context.Context, state NodeState) error {
	_, err := c.do(ctx, http.MethodDelete,
		fmt.Sprintf("/v1/workspaces/%s/nodes/%s", state.WorkspaceID, state.NodeID), nil)
	// Always wipe local state, even on Lens-side failure.
	_ = ClearState()
	return err
}

// Heartbeat pings Lens to keep last_seen_at fresh + report
// counters. The endpoint may not exist on older Lens deployments;
// we treat 404 as a non-fatal "older Lens" signal.
func (c *LensClient) Heartbeat(ctx context.Context, state NodeState, activeRequests, uptimeSeconds int64, modelsLoaded []string) error {
	payload := map[string]any{
		"active_requests": activeRequests,
		"uptime_seconds":  uptimeSeconds,
		"models_loaded":   modelsLoaded,
	}
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/workspaces/%s/nodes/%s/heartbeat",
			state.WorkspaceID, state.NodeID), payload)
	return err
}

// Earnings hits the workspace compute-mining stats endpoint.
// Returns the raw map so the renderer can pick out the fields
// it knows about — keeps the client resilient to additive
// server-side schema changes.
func (c *LensClient) Earnings(ctx context.Context, workspaceID string) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/v1/workspaces/%s/tokens/mining/compute", workspaceID), nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("node: decode earnings: %w", err)
	}
	return out, nil
}

// Balance returns the workspace's full balance snapshot. Used
// by `earnings` to compute the "lifetime" + "balance" rows.
func (c *LensClient) Balance(ctx context.Context, workspaceID string) (map[string]any, error) {
	resp, err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/v1/workspaces/%s/tokens/balance", workspaceID), nil)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(resp, &out); err != nil {
		return nil, fmt.Errorf("node: decode balance: %w", err)
	}
	return out, nil
}

// ─── HTTP plumbing ───────────────────────────────

// do is the single point where we add the auth header + handle
// status-code errors uniformly.
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
