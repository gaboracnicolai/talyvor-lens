package modelwatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// WebhookNotifier delivers an operator alert as an HMAC-signed POST.
//
// ⚠ WHY A SEPARATE SINK FROM alerts.Emitter, WHICH ALREADY POSTS TO A WEBHOOK. Because that emitter's
// wire contract is Track's SpendAlertPayload — workspace_id, feature, cost_usd, threshold — mirrored
// field-for-field from Track's own type. "A new model exists" has no workspace, no feature, no cost and
// no threshold. Sending it down that path would mean inventing values for four fields to satisfy a
// receiver that would then file it as a SPEND alert: a message whose fields are true of the transport
// and false of the data. That is the same failure as a caption that described a function rather than
// its data, and it would corrupt Track's spend records to save writing thirty lines.
//
// So this is its own contract, self-describing, with the reason for existing in the payload itself.
//
// SIGNING mirrors the Track emitter's scheme (hex HMAC-SHA256 over the exact bytes sent, in an
// X-Lens-Signature header) so a receiver already verifying Lens webhooks needs no new code path.
type WebhookNotifier struct {
	url    string
	secret string
	client *http.Client
}

// operatorAlert is the wire contract. Deliberately NOT shared with any other alert type: a shape reused
// across two meanings is how one of them ends up lying.
type operatorAlert struct {
	// Kind lets a receiver route without parsing prose. Stable string; treat as API.
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	EmittedAt string `json:"emitted_at"`
	// Source names the emitting subsystem so an operator reading the raw payload can find the code.
	Source string `json:"source"`
}

// NewWebhookNotifier returns nil when either url or secret is empty — matching NewEmitter's
// unconfigured-is-a-no-op convention.
//
// ⚠ A nil return is NOT silently benign here. modelwatch.Watcher.ReportReadiness treats a nil Notifier
// as a boot-time ERROR with the env vars named, and drops lens_modelwatch_sink_configured to 0, so an
// unconfigured sink is loud and alertable rather than an alert that quietly never fires. That distinction
// is the whole point: the Track spend emitter follows the same nil convention and is unconfigured in
// every deploy surface in this repo, which means no spend alert has ever actually been delivered — and
// nothing anywhere says so.
//
// The URL must be absolute http(s); a malformed one is rejected here rather than failing hourly at
// delivery time, when nobody is watching.
func NewWebhookNotifier(rawURL, secret string) (*WebhookNotifier, error) {
	if rawURL == "" || secret == "" {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("alert webhook URL is unparseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("alert webhook URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("alert webhook URL has no host: %q", rawURL)
	}
	return &WebhookNotifier{
		url:    rawURL,
		secret: secret,
		client: &http.Client{Timeout: listTimeout},
	}, nil
}

// Describe names the sink WITHOUT its secret and without query parameters, since webhook URLs
// frequently carry a token in the query string (Slack and Teams both do) and this string is logged.
func (n *WebhookNotifier) Describe() string {
	if n == nil {
		return "none"
	}
	u, err := url.Parse(n.url)
	if err != nil {
		return "webhook (unparseable url)"
	}
	return "webhook " + u.Scheme + "://" + u.Host + u.Path
}

// Notify delivers one alert synchronously. The caller runs on a background poller with nothing waiting
// on it, so unlike the serve-path spend emitter there is no reason to spawn a goroutine here — and a
// synchronous call is what lets a delivery failure be REPORTED rather than dropped, which matters when
// the whole purpose is that a person finds out.
func (n *WebhookNotifier) Notify(ctx context.Context, subject, body string) error {
	if n == nil {
		return fmt.Errorf("no alert sink configured")
	}
	payload := operatorAlert{
		Kind:      "catalog_drift",
		Severity:  "error",
		Subject:   subject,
		Body:      body,
		EmittedAt: time.Now().UTC().Format(time.RFC3339),
		Source:    "lens/internal/modelwatch",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lens-Signature", n.sign(raw))
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// 2xx only. A 3xx is not a delivery: following a redirect on a signed POST would re-send the body
	// to a host the signature was not computed for, and a 404 from a stale webhook must read as failure
	// rather than as "sent".
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("alert webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// sign computes the hex HMAC-SHA256 over the EXACT bytes posted — the same scheme as
// alerts.Emitter.sign, so an existing Lens webhook receiver verifies this without changes.
func (n *WebhookNotifier) sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(n.secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
