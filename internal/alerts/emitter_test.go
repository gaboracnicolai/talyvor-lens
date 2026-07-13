package alerts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sampleAlert() Alert {
	return Alert{
		Level: AlertCritical, Team: "team-x", Feature: "ENG-42",
		Message: "Critical spend threshold exceeded", SpendUSD: 12.50, Threshold: 10.00,
		CreatedAt: time.Now().UTC(), WorkspaceID: "ws-1",
	}
}

// trackVerify REPLICATES Track's lensintegration.verifySignature EXACTLY (the
// source of truth; separate module, so replicated not imported): the header is
// hex(HMAC_SHA256(secret, body)), constant-time compared. If this accepts our
// signature, Track's real verifier does too.
func trackVerify(body []byte, providedHex, secret string) bool {
	if providedHex == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(expected), []byte(providedHex)) == 1
}

// trackPayload mirrors Track's SpendAlertPayload tags for decode-side assertions.
type trackPayload struct {
	Type        string  `json:"type"`
	WorkspaceID string  `json:"workspace_id"`
	Feature     string  `json:"feature"`
	CostUSD     float64 `json:"cost_usd"`
	Threshold   float64 `json:"threshold"`
	EventID     string  `json:"event_id"`
	EmittedAt   string  `json:"emitted_at"`
}

type capture struct {
	body []byte
	sig  string
	ct   string
}

func captureServer(t *testing.T) (*httptest.Server, chan capture) {
	t.Helper()
	ch := make(chan capture, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- capture{body: b, sig: r.Header.Get("X-Lens-Signature"), ct: r.Header.Get("Content-Type")}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv, ch
}

func awaitCapture(t *testing.T, ch chan capture) capture {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("Track never received an emit")
		return capture{}
	}
}

// (a) THE INVARIANT — a hung/unreachable Track must NEVER block the serve path.
// Emit is async fire-and-forget: it returns immediately even when Track hangs
// forever. (Proven a LIVE guard by the mutation check: making Emit synchronous
// fails this — see the report.)
func TestEmitter_Emit_NeverBlocks(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // black hole: never responds until the test ends
	}))
	defer srv.Close()
	defer close(release)

	e := NewEmitter(srv.URL, "secret")
	start := time.Now()
	e.Emit(sampleAlert())
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Emit blocked for %v against a hung Track — a spend alert must NEVER block the serve path", d)
	}
}

// (a') Same invariant through the ACTUAL fire path a serve uses: fireAlert (which
// recordSpend calls) must not block on a hung Track. nc=nil is fine — the emit
// runs before the NATS check.
func TestFireAlert_NeverBlocksOnHungTrack(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	am := newAlertManager(nil, nil, nil)
	am.SetEmitter(NewEmitter(srv.URL, "secret"))
	start := time.Now()
	_ = am.fireAlert(context.Background(), sampleAlert())
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("fireAlert blocked for %v — the serve path (recordSpend→evaluateRule→fireAlert) must never block on Track", d)
	}
}

// (b) A valid emit produces a body Track's REAL verifier accepts + the exact contract.
func TestEmitter_ValidEmit_AcceptedByTrackVerifier(t *testing.T) {
	const secret = "shared-secret"
	srv, ch := captureServer(t)
	defer srv.Close()

	NewEmitter(srv.URL, secret).Emit(sampleAlert())
	c := awaitCapture(t, ch)

	if !trackVerify(c.body, c.sig, secret) {
		t.Fatalf("Track's verifier would REJECT the signature — the signing scheme does not mirror Track")
	}
	if c.ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", c.ct)
	}
	var p trackPayload
	if err := json.Unmarshal(c.body, &p); err != nil {
		t.Fatalf("body is not valid JSON for Track's contract: %v", err)
	}
	if p.Type != "spend_alert" || p.WorkspaceID != "ws-1" || p.Feature != "ENG-42" || p.CostUSD != 12.50 || p.Threshold != 10.00 {
		t.Errorf("payload mismatch vs Track contract: %+v", p)
	}
	if p.EventID == "" {
		t.Error("event_id must be set (activates Track's durable dedup)")
	}
	if ts, err := time.Parse(time.RFC3339, p.EmittedAt); err != nil {
		t.Errorf("emitted_at %q not RFC3339: %v", p.EmittedAt, err)
	} else if time.Since(ts) > time.Minute {
		t.Errorf("emitted_at %v not fresh", ts)
	}
}

// (c) event_id is UNIQUE per emit; emitted_at is fresh.
func TestEmitter_EventIDUniquePerEmit(t *testing.T) {
	srv, ch := captureServer(t)
	defer srv.Close()
	e := NewEmitter(srv.URL, "s")

	e.Emit(sampleAlert())
	e.Emit(sampleAlert())
	c1 := awaitCapture(t, ch)
	c2 := awaitCapture(t, ch)

	var p1, p2 trackPayload
	_ = json.Unmarshal(c1.body, &p1)
	_ = json.Unmarshal(c2.body, &p2)
	if p1.EventID == "" || p2.EventID == "" {
		t.Fatal("event_id empty")
	}
	if p1.EventID == p2.EventID {
		t.Fatalf("event_id NOT unique per emit: both %q — Track would wrongly dedup two distinct alerts", p1.EventID)
	}
	for _, p := range []trackPayload{p1, p2} {
		if ts, err := time.Parse(time.RFC3339, p.EmittedAt); err != nil || time.Since(ts) > time.Minute {
			t.Errorf("emitted_at not fresh/valid: %q (%v)", p.EmittedAt, err)
		}
	}
}

// (d) No URL/secret configured → emitter disabled, nothing fires, no error/panic.
func TestEmitter_Disabled_NoOp(t *testing.T) {
	if NewEmitter("", "secret") != nil {
		t.Error("empty URL must disable the emitter (nil)")
	}
	if NewEmitter("http://x", "") != nil {
		t.Error("empty secret must disable the emitter (nil)")
	}
	var e *Emitter        // nil
	e.Emit(sampleAlert()) // must not panic

	// Via the fire path: a disabled emitter fires NO request.
	hit := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- struct{}{} }))
	defer srv.Close()
	am := newAlertManager(nil, nil, nil)
	am.SetEmitter(NewEmitter("", "")) // disabled → nil
	_ = am.fireAlert(context.Background(), sampleAlert())
	select {
	case <-hit:
		t.Fatal("a disabled emitter fired a request")
	case <-time.After(200 * time.Millisecond):
		// good — nothing fired
	}
}
