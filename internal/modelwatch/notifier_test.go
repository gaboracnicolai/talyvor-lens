package modelwatch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookNotifier_SignsAndSendsASelfDescribingPayload(t *testing.T) {
	const secret = "s3cr3t"
	var gotBody []byte
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSig = r.Header.Get("X-Lens-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n, err := NewWebhookNotifier(srv.URL, secret)
	if err != nil || n == nil {
		t.Fatalf("NewWebhookNotifier: %v (nil=%v)", err, n == nil)
	}
	if err := n.Notify(context.Background(), "subj", "body text"); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	if want := hex.EncodeToString(mac.Sum(nil)); gotSig != want {
		t.Errorf("signature over the exact bytes sent = %q, want %q", gotSig, want)
	}

	var p operatorAlert
	if err := json.Unmarshal(gotBody, &p); err != nil {
		t.Fatalf("payload is not the operator-alert contract: %v", err)
	}
	// The payload must say what it IS, so a receiver never has to infer the meaning from prose — and
	// specifically so this can never be mistaken for, or routed as, a SPEND alert.
	if p.Kind != "catalog_drift" {
		t.Errorf("kind = %q, want catalog_drift", p.Kind)
	}
	if p.Source == "" || p.EmittedAt == "" || p.Subject != "subj" || p.Body != "body text" {
		t.Errorf("payload is not self-describing: %+v", p)
	}
}

// ⚠ Unconfigured must be an EXPLICIT nil, because the caller's whole anti-inert guarantee keys off it.
func TestNewWebhookNotifier_UnconfiguredIsNil(t *testing.T) {
	for _, c := range []struct{ url, secret string }{
		{"", ""}, {"https://example.test/h", ""}, {"", "s"},
	} {
		n, err := NewWebhookNotifier(c.url, c.secret)
		if err != nil {
			t.Errorf("(%q,%q): unexpected error %v", c.url, c.secret, err)
		}
		if n != nil {
			t.Errorf("(%q,%q): want nil notifier when either half is unset", c.url, c.secret)
		}
	}
}

// A malformed URL must fail at construction — at boot, where someone is watching — not hourly at
// delivery time where the failure is a log line among thousands.
func TestNewWebhookNotifier_RejectsMalformedURLAtConstruction(t *testing.T) {
	for _, bad := range []string{"not-a-url", "ftp://example.test/h", "https://"} {
		if _, err := NewWebhookNotifier(bad, "s"); err == nil {
			t.Errorf("%q was accepted as an alert sink", bad)
		}
	}
}

// Describe is LOGGED, so it must not leak the secret — and webhook URLs routinely carry a token in the
// query string (Slack and Teams both do), which is why the whole query is dropped, not just the secret.
func TestDescribe_LeaksNeitherSecretNorQueryToken(t *testing.T) {
	n, err := NewWebhookNotifier("https://hooks.example.test/services/T000/B000/xoxb-TOKEN?token=QUERYSECRET", "MYSECRET")
	if err != nil {
		t.Fatal(err)
	}
	d := n.Describe()
	if strings.Contains(d, "MYSECRET") {
		t.Errorf("Describe leaked the HMAC secret: %q", d)
	}
	if strings.Contains(d, "QUERYSECRET") {
		t.Errorf("Describe leaked a query-string token: %q", d)
	}
	if !strings.Contains(d, "hooks.example.test") {
		t.Errorf("Describe is useless — it does not name the host: %q", d)
	}
}

// A non-2xx is a FAILED delivery. A redirect in particular must not count as sent: following it would
// re-post the signed body to a host the signature was not computed for.
func TestNotify_NonSuccessIsAFailure(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		n, _ := NewWebhookNotifier(srv.URL, "s")
		if err := n.Notify(context.Background(), "s", "b"); err == nil {
			t.Errorf("HTTP %d was treated as a successful delivery", code)
		}
		srv.Close()
	}
}

// ⚠ THE TYPED-NIL TRAP, asserted rather than trusted to a comment. A (*WebhookNotifier)(nil) stored in
// a Notifier interface is NOT == nil, so the readiness check would report an armed sink and the
// anti-inert guarantee would invert into the exact bug it exists to prevent. cmd/lens converts
// explicitly; this pins why that is necessary.
func TestTypedNilNotifierIsNotNilInterface(t *testing.T) {
	var concrete *WebhookNotifier
	var iface Notifier = concrete
	if iface == nil {
		t.Skip("Go semantics changed; the guard in cmd/lens is no longer needed")
	}
	// Given that, a nil *WebhookNotifier must at least refuse to claim it delivered anything.
	if err := concrete.Notify(context.Background(), "s", "b"); err == nil {
		t.Error("a nil notifier reported a successful delivery — an alert would be silently dropped " +
			"while the readiness check reported the sink armed")
	}
	if got := concrete.Describe(); got != "none" {
		t.Errorf("nil notifier Describe() = %q, want \"none\"", got)
	}
}
