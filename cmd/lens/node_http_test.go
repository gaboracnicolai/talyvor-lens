package main

import (
	"crypto/tls"
	"net/http"
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
