package main

// server.go — the local HTTP server the node binary spins up.
// Lens makes inference requests against this endpoint when it
// routes work to the operator's GPU. Three routes:
//   POST /inference  (X-Node-Secret required)
//   GET  /health
//   GET  /models

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/talyvor/lens/internal/povi"
)

// InferenceServer is the small HTTP layer in front of the local
// provider. Holds the secret used to verify Lens-originated
// requests and a counter of in-flight calls for /health.
type InferenceServer struct {
	provider Provider
	secret   string
	cfg      NodeConfig

	// signer (optional) produces a signed PoVI receipt per served request;
	// lens (optional) submits it to Lens for verification + audit. Both nil
	// for a node without a signing key.
	signer *receiptSigner
	lens   *LensClient

	mu             sync.Mutex
	activeRequests int64
	startedAt      time.Time
	// modelsCache is refreshed each /models call (kept simple —
	// the heartbeat will re-fetch every 30s anyway).
	modelsCache []string
}

// SetReceiptSigner wires PoVI receipt production. A nil signer disables
// receipts (older nodes); a nil lens disables the async submit (the receipt is
// still returned in the response).
func (s *InferenceServer) SetReceiptSigner(rs *receiptSigner, lens *LensClient) {
	s.signer = rs
	s.lens = lens
}

func NewInferenceServer(provider Provider, secret string, cfg NodeConfig) *InferenceServer {
	return &InferenceServer{
		provider:  provider,
		secret:    secret,
		cfg:       cfg,
		startedAt: time.Now(),
	}
}

// Handler wires the three routes onto an http.ServeMux. Kept
// separate from ListenAndServe so tests can mount it on a
// httptest.Server.
func (s *InferenceServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/inference", s.handleInference)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/models", s.handleModels)
	return mux
}

// ─── inference ───────────────────────────────────

func (s *InferenceServer) handleInference(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Shared-secret auth — verifies Lens (not a random caller) is
	// driving the request. Empty configured secret means the
	// operator's running in "registered without secret" mode (e.g.
	// an older Lens that doesn't issue secrets); we still accept
	// requests then, but log a warning at startup.
	if s.secret != "" && r.Header.Get("X-Node-Secret") != s.secret {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var req InferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Model == "" {
		http.Error(w, "model required", http.StatusBadRequest)
		return
	}

	atomic.AddInt64(&s.activeRequests, 1)
	defer atomic.AddInt64(&s.activeRequests, -1)

	resp, err := s.provider.Infer(r.Context(), req)
	if err != nil {
		http.Error(w, "inference failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// PoVI: produce a signed receipt attesting to this response (attestation +
	// tamper-evidence, NOT proof of honest computation). Returned in the body
	// and best-effort submitted to Lens off the response path.
	if s.signer != nil {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = "req_" + generateSecret()
		}
		rec := s.signer.sign(reqID, req.Model, resp.InputTokens, resp.OutputTokens, resp.Text)
		resp.Receipt = &rec
		if s.lens != nil {
			go func(rc povi.Receipt, ws string) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.lens.SubmitReceipt(ctx, ws, rc)
			}(rec, s.signer.workspaceID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// ─── health ──────────────────────────────────────

func (s *InferenceServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status := "healthy"
	if err := s.provider.Health(probeCtx); err != nil {
		status = "unhealthy"
	}
	out := map[string]any{
		"status":          status,
		"models":          s.cfg.Models,
		"gpu_type":        s.cfg.GPUType,
		"provider":        s.cfg.Provider,
		"active_requests": atomic.LoadInt64(&s.activeRequests),
		"uptime_seconds":  int64(time.Since(s.startedAt).Seconds()),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// ─── models ──────────────────────────────────────

func (s *InferenceServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	names, err := s.provider.ListModels(ctx)
	if err != nil {
		http.Error(w, "list models: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.mu.Lock()
	s.modelsCache = names
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": names})
}

// ─── start / shutdown ────────────────────────────

// ListenAndServe is the convenience wrapper main.go uses. Logs
// the listen address and returns the underlying http.Server so
// the caller can Shutdown on SIGTERM.
func (s *InferenceServer) ListenAndServe(port int) (*http.Server, error) {
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("✅ Talyvor Node listening on port %d", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("node: HTTP server error: %v", err)
		}
	}()
	return server, nil
}

// ActiveRequests is a thread-safe accessor used by the heartbeat
// goroutine.
func (s *InferenceServer) ActiveRequests() int64 {
	return atomic.LoadInt64(&s.activeRequests)
}

// UptimeSeconds is similar.
func (s *InferenceServer) UptimeSeconds() int64 {
	return int64(time.Since(s.startedAt).Seconds())
}
