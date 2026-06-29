package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CHARACTERIZATION TEST (PR-3b step a) — pins the CURRENT header/auth behavior of forward (proxy.go:1883)
// on the credential path, which recon flagged as untested. It encodes what forward DOES today (not what
// it "should" do): the header-copy loop forwards client headers EXCEPT Host, then cfg.setAuth runs AFTER
// the copy and OVERWRITES the Authorization with the configured provider key. When forward's round-trip
// body moves to internal/inference in step (b), THIS test stays in package proxy as the behavior oracle —
// a verbatim move must keep it green.
//
// If forward's behavior were ever to change here (e.g. stop overwriting client auth, or start leaking the
// Host), this test turns red — which on the credential path is exactly what we want a tripwire for.
func TestForward_HeaderAuthBehavior_Characterization(t *testing.T) {
	// What the upstream actually received (captured over a channel for a happens-before, -race-clean read).
	type received struct {
		authorization string
		clientTrace   string
		host          string
	}
	got := make(chan received, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		got <- received{
			authorization: rr.Header.Get("Authorization"),
			clientTrace:   rr.Header.Get("X-Client-Trace"),
			host:          rr.Host, // Go puts the request Host on r.Host, not r.Header
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	// openai provider config points at the httptest upstream; the configured key is "openai-key".
	p := newProxyWithFallback(t, upstream.URL, "", "")
	cfg := p.configForProvider("openai")

	// Inbound CLIENT request carrying: junk Authorization, a benign custom header, and a client Host (both
	// as r.Host and as an explicit "Host" header — the loop must skip BOTH paths).
	body := `{"model":"gpt-4o","messages":[]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer CLIENT-JUNK-KEY")
	r.Header.Set("X-Client-Trace", "trace-123")
	r.Header.Set("Host", "client.example.com")
	r.Host = "client.example.com"

	resp, _, _, err := p.forward(context.Background(), r, []byte(body), "gpt-4o", cfg)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	_ = resp.Body.Close()

	up := <-got
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	// Print the PINNED behavior so it's visible in the run.
	t.Logf("PINNED forward header/auth behavior — upstream received:")
	t.Logf("  Authorization  = %q  (configured provider key; client junk gone)", up.authorization)
	t.Logf("  X-Client-Trace = %q  (client custom header forwarded)", up.clientTrace)
	t.Logf("  Host           = %q  (upstream host %q; client Host NOT forwarded)", up.host, upstreamHost)

	// 1. Client custom header IS copied onto the upstream request.
	if up.clientTrace != "trace-123" {
		t.Errorf("X-Client-Trace: upstream got %q, want %q (client headers are forwarded)", up.clientTrace, "trace-123")
	}
	// 2. The Host is NOT copied — upstream Host is the provider's host, not the client's.
	if up.host == "client.example.com" {
		t.Errorf("Host: upstream got the CLIENT host %q — forward must NOT forward Host", up.host)
	}
	if up.host != upstreamHost {
		t.Errorf("Host: upstream got %q, want the upstream host %q", up.host, upstreamHost)
	}
	// 3. cfg.setAuth OVERWRITES the client Authorization with the configured provider key (setAuth runs
	//    AFTER the header copy and wins).
	if up.authorization == "Bearer CLIENT-JUNK-KEY" {
		t.Errorf("Authorization: client junk leaked upstream (%q) — setAuth must overwrite it", up.authorization)
	}
	if up.authorization != "Bearer openai-key" {
		t.Errorf("Authorization: upstream got %q, want the configured %q (setAuth wins over the client header)", up.authorization, "Bearer openai-key")
	}
	// 4. Observable end-state, asserted together: configured auth present, client junk gone, client custom
	//    header present, Host = upstream. (Points 1–3 above collectively encode this.)
}
