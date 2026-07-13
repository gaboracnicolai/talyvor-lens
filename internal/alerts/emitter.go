package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// emitTimeout bounds a single Track POST so a hung Track self-limits and the
// emit goroutine can never leak indefinitely. It is deliberately short — the
// emit is advisory and off the serve path, so there is no value in waiting long.
const emitTimeout = 5 * time.Second

// spendAlertPayload is the Lens→Track spend-alert wire contract. It MIRRORS
// Track's lensintegration.SpendAlertPayload EXACTLY (the source of truth): the
// JSON tags must match, and the two SEC-7 fields — EventID + EmittedAt — are what
// activate Track's durable event_id dedup and freshness window (the whole point).
//
// cost_usd / threshold are Tier-3 USD floats (advisory cost accounting), NOT
// conserved µLENS tokens — SEC-2's money types are untouched by this path.
type spendAlertPayload struct {
	Type        string  `json:"type"`
	WorkspaceID string  `json:"workspace_id"`
	Feature     string  `json:"feature"`
	CostUSD     float64 `json:"cost_usd"`
	Threshold   float64 `json:"threshold"`
	EventID     string  `json:"event_id"`
	EmittedAt   string  `json:"emitted_at"`
}

// Emitter POSTs spend alerts to Track's webhook, HMAC-signed. It is the outbound
// half of SEC-7: Track's receiver already dedups on event_id and enforces a
// freshness window; this is what finally SENDS them.
//
// ┌─ SECURITY INVARIANT (hard, structural) ─────────────────────────────────────┐
// │ A spend alert may NEVER block, delay, or fail a serve. Emit spawns a         │
// │ goroutine with a background-derived, timeout-bounded context — NEVER the     │
// │ caller's request context — so a slow / hung / unreachable Track cannot       │
// │ couple into serve latency or availability. A notification path that can      │
// │ block a serve is a GRIEFING VECTOR: whoever can slow Track could throttle    │
// │ Lens. Every failure is FAIL-OPEN (logged, dropped): no retry, no queue, no   │
// │ dead-letter — advisory cost accounting, and Track's dedup makes at-least-    │
// │ once unnecessary.                                                            │
// └──────────────────────────────────────────────────────────────────────────────┘
type Emitter struct {
	url    string
	secret string
	client *http.Client
}

// NewEmitter builds the Track spend-alert emitter. An empty url OR secret returns
// a nil *Emitter — a disabled, total no-op (Emit on a nil receiver is safe). This
// is the default posture (unset config ⇒ nothing fires, no error).
func NewEmitter(url, secret string) *Emitter {
	if url == "" || secret == "" {
		return nil
	}
	return &Emitter{
		url:    url,
		secret: secret,
		client: &http.Client{Timeout: emitTimeout},
	}
}

// Emit sends one spend alert to Track asynchronously, fire-and-forget. It RETURNS
// IMMEDIATELY — the payload is stamped here (so event_id/emitted_at reflect this
// call) but the HTTP delivery runs on its own goroutine off the request path (see
// the type's SECURITY INVARIANT). A nil/disabled Emitter is a no-op.
func (e *Emitter) Emit(alert Alert) {
	if e == nil {
		return
	}
	p := spendAlertPayload{
		Type:        "spend_alert",
		WorkspaceID: alert.WorkspaceID,
		Feature:     alert.Feature,
		CostUSD:     alert.SpendUSD,
		Threshold:   alert.Threshold,
		EventID:     uuid.NewString(),                      // unique per emit; server-generated, never from a caller
		EmittedAt:   time.Now().UTC().Format(time.RFC3339), // server clock; activates Track's freshness window
	}
	// INVARIANT: deliver on our OWN goroutine — the serve has already moved on.
	go e.post(p)
}

// post marshals, signs, and delivers one payload. Runs ONLY on Emit's goroutine.
// Fail-open: every error is logged and dropped, never surfaced to a caller.
func (e *Emitter) post(p spendAlertPayload) {
	body, err := json.Marshal(p)
	if err != nil {
		slog.Warn("alerts: track emit marshal failed (dropped)", slog.String("err", err.Error()))
		return
	}
	// Its OWN background-derived, timeout-bounded context — NEVER a request context —
	// so a hung Track self-limits and this goroutine can never leak indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), emitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		slog.Warn("alerts: track emit request build failed (dropped)", slog.String("err", err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lens-Signature", sign(body, e.secret))
	resp, err := e.client.Do(req)
	if err != nil {
		slog.Warn("alerts: track emit POST failed (fail-open, dropped)",
			slog.String("feature", p.Feature), slog.String("err", err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		slog.Warn("alerts: track emit non-2xx (fail-open, dropped)",
			slog.String("feature", p.Feature), slog.Int("status", resp.StatusCode))
	}
}

// sign mirrors Track's lensintegration.verifySignature EXACTLY: the header value
// is hex(HMAC_SHA256(secret, body)), which Track constant-time-compares against.
func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
