package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewNodeHTTPClient_DefaultTimeout(t *testing.T) {
	c := newNodeHTTPClient(false, 0)
	if c.Timeout != 5*time.Second {
		t.Fatalf("expected 5s default timeout, got %v", c.Timeout)
	}
}

func TestNewNodeHTTPClient_CustomTimeout(t *testing.T) {
	c := newNodeHTTPClient(false, 10*time.Second)
	if c.Timeout != 10*time.Second {
		t.Fatalf("expected 10s timeout, got %v", c.Timeout)
	}
}

func TestNewNodeHTTPClient_SkipVerifyFalse(t *testing.T) {
	c := newNodeHTTPClient(false, 5*time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("expected non-nil TLSClientConfig")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be false when skipVerify=false")
	}
}

func TestNewNodeHTTPClient_SkipVerifyTrue(t *testing.T) {
	c := newNodeHTTPClient(true, 5*time.Second)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("expected non-nil TLSClientConfig")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be true when skipVerify=true")
	}
}

func TestNewNodeHTTPClient_MinTLSVersion(t *testing.T) {
	c := newNodeHTTPClient(false, 5*time.Second)
	tr := c.Transport.(*http.Transport)
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected TLS 1.2 minimum, got %v", tr.TLSClientConfig.MinVersion)
	}
}

// B-SSRF-Lens: node registration + node inference/attestation/challenge/benchprobe dial a
// caller-supplied node URL through newNodeHTTPClient, which had no SSRF guard — so a registrant could
// point it at the cloud metadata endpoint (169.254.169.254), the Lens server's loopback, or a private
// address. RED: the client reaches an internal (loopback) address. GREEN: it refuses to dial it.
func TestNewNodeHTTPClient_SSRF_RefusesInternal(t *testing.T) {
	var hits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	client := newNodeHTTPClient(false, 3*time.Second)
	resp, err := client.Get(internal.URL) // internal.URL is http://127.0.0.1:PORT — a loopback address
	if resp != nil {
		_ = resp.Body.Close()
	}

	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("SSRF: node client REACHED internal loopback %s (%d hits) — must be blocked before connect", internal.URL, hits)
	}
	if err == nil {
		t.Errorf("SSRF: node client to an internal address returned nil error — want a blocked-address error")
	}
}
