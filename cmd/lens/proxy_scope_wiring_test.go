package main

import (
	"os"
	"strings"
	"testing"
)

// Wiring guard: every LLM proxy route (the serving surface) must be registered
// THROUGH the proxy-scope guard. Until this PR all four scopes were dead —
// RequireScope had zero callers — so a key lacking the proxy scope could still
// drive the proxy. This test fails on main (routes are bare authed.Post) and
// passes once each proxy registration goes through auth.RequireScope(auth.ScopeProxy).
func TestProxyRoutesAreScopeGuarded(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	if !strings.Contains(text, "auth.RequireScope(auth.ScopeProxy)") {
		t.Fatal("proxy-scope guard is not defined in main.go: expected auth.RequireScope(auth.ScopeProxy)")
	}

	// Prefixes that uniquely identify an LLM proxy registration.
	proxyMarkers := []string{
		`.Post("/v1/proxy/`,
		`.Post("/oai/*"`,
		`.Post("/anthropic/*"`,
	}
	proxyLines := 0
	var unguarded []string
	for _, line := range strings.Split(text, "\n") {
		isProxy := false
		for _, m := range proxyMarkers {
			if strings.Contains(line, m) {
				isProxy = true
				break
			}
		}
		if !isProxy {
			continue
		}
		proxyLines++
		// The registration must route through the proxy-scope middleware.
		if !strings.Contains(line, "proxyScope") && !strings.Contains(line, "RequireScope(auth.ScopeProxy)") {
			unguarded = append(unguarded, strings.TrimSpace(line))
		}
	}

	if proxyLines < 9 {
		t.Fatalf("expected >=9 proxy route registrations, found %d — did the proxy paths change?", proxyLines)
	}
	if len(unguarded) > 0 {
		t.Fatalf("these proxy routes are NOT scope-guarded (each must go through the proxy-scope middleware):\n  %s",
			strings.Join(unguarded, "\n  "))
	}
}
