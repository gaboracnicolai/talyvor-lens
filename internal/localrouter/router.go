package localrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/router"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	healthInterval   = 30 * time.Second
	healthTimeout    = 5 * time.Second
	generateTimeout  = 60 * time.Second
)

// modelPreference orders our preferred local model prefixes, best-first.
// Matched by prefix so tagged names like "llama3.2:latest" still match.
var modelPreference = []string{"llama3.2", "llama3", "mistral"}

type LocalRouter struct {
	ollamaURL  string
	httpClient *http.Client
	mu         sync.RWMutex
	available  bool
	models     []LocalModel
}

type LocalModel struct {
	Name        string  `json:"name"`
	SizeGB      float64 `json:"size"`
	IsAvailable bool    `json:"is_available"`
}

type LocalRoutingDecision struct {
	UseLocal bool
	Model    string
	Reason   string
}

// New builds a LocalRouter. It does NOT auto-start the health-check
// goroutine — callers (main.go) invoke StartHealthCheck explicitly so
// tests can run without a background ticker.
func New(ollamaURL string) *LocalRouter {
	if ollamaURL == "" {
		ollamaURL = defaultOllamaURL
	}
	return &LocalRouter{
		ollamaURL:  strings.TrimRight(ollamaURL, "/"),
		httpClient: &http.Client{Timeout: healthTimeout},
	}
}

// SetHTTPClient replaces the underlying HTTP client. main.go calls this
// to inject a client whose TLS config trusts node certificates (e.g.
// self-signed certs on LAN deployments, controlled via
// LENS_NODE_TLS_SKIP_VERIFY).
func (r *LocalRouter) SetHTTPClient(client *http.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.httpClient = client
}

type tagsResponse struct {
	Models []LocalModel `json:"models"`
}

// CheckAvailability hits /api/tags. On 200 it parses the model list and
// flips r.available=true; any error or non-200 marks it unavailable.
func (r *LocalRouter) CheckAvailability(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.ollamaURL+"/api/tags", nil)
	if err != nil {
		r.setAvailability(false, nil)
		return false
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.setAvailability(false, nil)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		r.setAvailability(false, nil)
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.setAvailability(false, nil)
		return false
	}
	var tags tagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		r.setAvailability(false, nil)
		return false
	}
	r.setAvailability(true, tags.Models)
	return true
}

func (r *LocalRouter) setAvailability(available bool, models []LocalModel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.available = available
	r.models = models
}

func (r *LocalRouter) StartHealthCheck(ctx context.Context) {
	// First check fires immediately so r.available reflects reality
	// before the first request arrives — otherwise ShouldUseLocal
	// would return false for the full first interval.
	r.CheckAvailability(ctx)
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.CheckAvailability(ctx)
		}
	}
}

func (r *LocalRouter) ShouldUseLocal(complexity router.RequestComplexity, wsID string) LocalRoutingDecision {
	r.mu.RLock()
	available := r.available
	models := append([]LocalModel(nil), r.models...)
	r.mu.RUnlock()

	if !available || len(models) == 0 {
		return LocalRoutingDecision{}
	}
	if complexity.Score() > 1 {
		return LocalRoutingDecision{}
	}
	if wsID != "" && wsID != "default" {
		return LocalRoutingDecision{}
	}

	chosen := pickModel(models)
	return LocalRoutingDecision{
		UseLocal: true,
		Model:    chosen,
		Reason:   "Simple query routed to local Ollama instance",
	}
}

// pickModel walks the preference list looking for a prefix match against
// any installed model; falls back to the first available model name.
func pickModel(models []LocalModel) string {
	for _, want := range modelPreference {
		for _, m := range models {
			if strings.HasPrefix(m.Name, want) {
				return m.Name
			}
		}
	}
	return models[0].Name
}

type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// Forward issues a one-shot non-streaming /api/generate call. Local models
// can be slow on consumer hardware, so the timeout is 60s — longer than
// our cloud-side default. Network/HTTP errors return non-nil so the proxy
// caller can fall back to cloud.
func (r *LocalRouter) Forward(ctx context.Context, model, prompt string) ([]byte, error) {
	payload, err := json.Marshal(generateRequest{Model: model, Prompt: prompt, Stream: false})
	if err != nil {
		return nil, fmt.Errorf("localrouter: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ollamaURL+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("localrouter: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: generateTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("localrouter: forward: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("localrouter: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("localrouter: ollama status %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// FormatAsOpenAI wraps an Ollama /api/generate response in OpenAI's
// chat-completion shape. Downstream code can then treat local responses
// identically to cloud responses for caching and event recording.
func (r *LocalRouter) FormatAsOpenAI(ollama []byte, model string) ([]byte, error) {
	var parsed ollamaResponse
	if err := json.Unmarshal(ollama, &parsed); err != nil {
		return nil, fmt.Errorf("localrouter: decode ollama response: %w", err)
	}
	if !parsed.Done {
		return nil, errors.New("localrouter: ollama response not done")
	}

	out := map[string]any{
		"id":      "local-" + uuid.NewString(),
		"object":  "chat.completion",
		"model":   model,
		"choices": []map[string]any{{
			"message": map[string]any{
				"role":    "assistant",
				"content": parsed.Response,
			},
			"finish_reason": "stop",
		}},
	}
	formatted, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("localrouter: marshal openai-shape: %w", err)
	}
	return formatted, nil
}

// Available exposes the current cached availability for callers that
// only need a fast read (e.g. for metrics or admin endpoints).
func (r *LocalRouter) Available() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.available
}

// Models returns a copy of the installed-model list.
func (r *LocalRouter) Models() []LocalModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]LocalModel(nil), r.models...)
}

