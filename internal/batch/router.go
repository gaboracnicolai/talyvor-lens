package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
)

const (
	defaultAnthropicURL = "https://api.anthropic.com"
	pollInterval        = 5 * time.Minute
	httpTimeout         = 30 * time.Second
	// batchDiscount is the published Anthropic batch-pricing reduction.
	batchDiscount = 0.5
)

// pgxDB is the subset of *pgxpool.Pool that the batch router needs.
// nil pool keeps the in-memory pending map alive but skips persistence.
type pgxDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type BatchRouter struct {
	pool         pgxDB
	httpClient   *http.Client
	anthropicKey string
	anthropicURL string
	mu           sync.RWMutex
	pending      map[string]*BatchJob // requestID → job
}

type BatchJob struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspace_id"` // owner; the batch_jobs row already carries it (set at Submit)
	RequestID   string      `json:"request_id"`
	PromptHash  string      `json:"prompt_hash"`
	Provider    string      `json:"provider"`
	Model       string      `json:"model"`
	Prompt      string      `json:"prompt"`
	Status      BatchStatus `json:"status"`
	Response    []byte      `json:"response,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	CostUSD     float64     `json:"cost_usd"`
}

type BatchStatus string

const (
	BatchPending    BatchStatus = "pending"
	BatchProcessing BatchStatus = "processing"
	BatchComplete   BatchStatus = "complete"
	BatchFailed     BatchStatus = "failed"
)

type BatchEligibility struct {
	Eligible bool
	Reason   string
}

func New(pool *pgxpool.Pool, anthropicKey string) *BatchRouter {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newBatchRouter(db, anthropicKey)
}

func newBatchRouter(pool pgxDB, anthropicKey string) *BatchRouter {
	return &BatchRouter{
		pool:         pool,
		httpClient:   &http.Client{Timeout: httpTimeout},
		anthropicKey: anthropicKey,
		anthropicURL: defaultAnthropicURL,
		pending:      make(map[string]*BatchJob),
	}
}

// IsEligible returns whether a request can run through Anthropic's batch
// API. The caller is responsible for setting `batch_eligible: true` in
// the body when the X-Talyvor-Batch header was set on the HTTP request
// — that's how the header maps into this function's body-only signature.
func (r *BatchRouter) IsEligible(body []byte, wsID string) BatchEligibility {
	if wsID == "" {
		return BatchEligibility{Reason: "workspace id required"}
	}
	var m struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		BatchEligible bool   `json:"batch_eligible"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return BatchEligibility{Reason: "invalid body: " + err.Error()}
	}
	if !strings.HasPrefix(m.Model, "claude-") {
		return BatchEligibility{Reason: "batch API is anthropic-only"}
	}
	if m.Stream {
		return BatchEligibility{Reason: "streaming is not supported in batch mode"}
	}
	if !m.BatchEligible {
		return BatchEligibility{Reason: "batch_eligible flag not set"}
	}
	return BatchEligibility{Eligible: true}
}

const insertBatchJobSQL = `INSERT INTO batch_jobs
  (id, request_id, batch_id, workspace_id, provider, model, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

type submitRequest struct {
	Requests []submitRequestItem `json:"requests"`
}

type submitRequestItem struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

type submitResponse struct {
	ID string `json:"id"`
}

// Submit fires the body off to Anthropic's batches endpoint and registers
// a BatchJob in both the in-memory pending map and the batch_jobs table.
// Returns the job with the Anthropic batch ID populated; the poller is
// responsible for moving the job to BatchComplete later.
func (r *BatchRouter) Submit(ctx context.Context, wsID, model, prompt string, body []byte) (*BatchJob, error) {
	requestID := uuid.NewString()

	payload, err := json.Marshal(submitRequest{
		Requests: []submitRequestItem{{
			CustomID: requestID,
			Params:   body,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("batch: marshal submit: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.anthropicURL+"/v1/messages/batches", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("batch: build request: %w", err)
	}
	r.applyAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("batch: upstream: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("batch: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("batch: upstream status %d: %s", resp.StatusCode, snippet(respBody))
	}

	var apiResp submitResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("batch: decode response: %w", err)
	}
	if apiResp.ID == "" {
		return nil, fmt.Errorf("batch: upstream returned no id (body=%s)", snippet(respBody))
	}

	job := &BatchJob{
		ID:          apiResp.ID,
		WorkspaceID: wsID,
		RequestID:   requestID,
		Provider:    "anthropic",
		Model:       model,
		Prompt:      prompt,
		Status:      BatchPending,
		CreatedAt:   time.Now().UTC(),
	}

	r.mu.Lock()
	r.pending[requestID] = job
	r.mu.Unlock()

	if r.pool != nil {
		if _, err := r.pool.Exec(ctx, insertBatchJobSQL,
			job.ID, job.RequestID, job.ID, wsID, job.Provider, job.Model, string(job.Status), job.CreatedAt,
		); err != nil {
			slog.Warn("batch: insert batch_jobs failed", slog.String("err", err.Error()))
		}
	}
	return job, nil
}

func (r *BatchRouter) applyAuth(req *http.Request) {
	req.Header.Set("x-api-key", r.anthropicKey)
	req.Header.Set("anthropic-version", "2023-06-01")
}

type pollResponse struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
}

// Poll asks Anthropic for the batch status and, when it's ended, fetches
// the JSONL results blob and extracts the content for the job's custom_id.
// Returns the in-memory job pointer; callers can hold onto it across
// multiple Poll invocations.
func (r *BatchRouter) Poll(ctx context.Context, batchID string) (*BatchJob, error) {
	job := r.jobByBatchID(batchID)
	if job == nil {
		return nil, fmt.Errorf("batch: unknown batch %q", batchID)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.anthropicURL+"/v1/messages/batches/"+batchID, nil)
	if err != nil {
		return job, fmt.Errorf("batch: build poll request: %w", err)
	}
	r.applyAuth(req)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return job, fmt.Errorf("batch: poll: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return job, fmt.Errorf("batch: read poll response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return job, fmt.Errorf("batch: poll status %d: %s", resp.StatusCode, snippet(body))
	}
	var pr pollResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return job, fmt.Errorf("batch: decode poll: %w", err)
	}

	if pr.ProcessingStatus != "ended" {
		r.mu.Lock()
		job.Status = BatchProcessing
		r.mu.Unlock()
		return job, nil
	}

	// Ended → pull the JSONL results.
	return r.fetchResults(ctx, batchID, job)
}

// resultLine matches Anthropic's per-request JSONL row. The "succeeded"
// branch carries a Message; "errored" leaves message empty and we mark
// the job failed.
type resultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	} `json:"result"`
}

func (r *BatchRouter) fetchResults(ctx context.Context, batchID string, job *BatchJob) (*BatchJob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.anthropicURL+"/v1/messages/batches/"+batchID+"/results", nil)
	if err != nil {
		return job, fmt.Errorf("batch: build results request: %w", err)
	}
	r.applyAuth(req)
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return job, fmt.Errorf("batch: results: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return job, fmt.Errorf("batch: read results: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return job, fmt.Errorf("batch: results status %d: %s", resp.StatusCode, snippet(raw))
	}

	// Each line is one JSON object; find the one matching our request ID.
	var matched *resultLine
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rl resultLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.CustomID == job.RequestID {
			matched = &rl
			break
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if matched == nil {
		job.Status = BatchFailed
		return job, fmt.Errorf("batch: no result for request_id %q", job.RequestID)
	}
	if matched.Result.Type != "succeeded" {
		job.Status = BatchFailed
		return job, fmt.Errorf("batch: request_id %q result type %q", job.RequestID, matched.Result.Type)
	}

	// Reassemble the response into an Anthropic message shape so downstream
	// code (proxy / cache) treats it like a normal /v1/messages reply.
	payload := map[string]any{"content": matched.Result.Message.Content}
	out, err := json.Marshal(payload)
	if err != nil {
		return job, err
	}
	now := time.Now().UTC()
	job.Status = BatchComplete
	job.Response = out
	job.CompletedAt = &now
	// Approximate token usage with the len/4 heuristic the rest of the
	// pipeline uses; price at the 50% batch rate.
	totalText := ""
	for _, c := range matched.Result.Message.Content {
		totalText += c.Text
	}
	job.CostUSD = alerts.CostUSD(job.Model, len(job.Prompt)/4, len(totalText)/4) * batchDiscount
	return job, nil
}

func (r *BatchRouter) jobByBatchID(batchID string) *BatchJob {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, j := range r.pending {
		if j.ID == batchID {
			return j
		}
	}
	return nil
}

// GetJobByRequestID returns the in-memory job for a request ID, or nil.
func (r *BatchRouter) GetJobByRequestID(requestID string) *BatchJob {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if j, ok := r.pending[requestID]; ok {
		return j
	}
	return nil
}

// ListJobs returns a snapshot of all in-memory batch jobs.
func (r *BatchRouter) ListJobs() []*BatchJob {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*BatchJob, 0, len(r.pending))
	for _, j := range r.pending {
		out = append(out, j)
	}
	return out
}

// StartPoller spawns the background polling goroutine.
func (r *BatchRouter) StartPoller(ctx context.Context) {
	r.pollLoop(ctx, pollInterval)
}

func (r *BatchRouter) pollLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pollAll(ctx)
		}
	}
}

func (r *BatchRouter) pollAll(ctx context.Context) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.pending))
	for _, j := range r.pending {
		if j.Status == BatchPending || j.Status == BatchProcessing {
			ids = append(ids, j.ID)
		}
	}
	r.mu.RUnlock()

	for _, id := range ids {
		if _, err := r.Poll(ctx, id); err != nil {
			slog.Warn("batch: Poll failed", slog.String("batch_id", id), slog.String("err", err.Error()))
		}
	}
}

func snippet(b []byte) string {
	s := string(b)
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return strings.ReplaceAll(s, "\n", " ")
}
