package proxy

import (
	"net"
	"net/url"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// testHermeticWorkspace is a gate-open workspace; the guard below is about the
// proxy's dial targets, not about any particular policy.
func testHermeticWorkspace() workspace.Workspace {
	return workspace.Workspace{
		ID: "ws-hermetic", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}
}

// NO TEST IN THIS PACKAGE MAY BE ONE FALLBACK AWAY FROM THE PUBLIC INTERNET.
//
// ⚠ HOW THIS WAS FOUND — BY MEASUREMENT, NOT BY READING. A new test pointed its
// "provider" at an httptest server that answers 500, to check what the compression
// measurement records when a request never reaches the spend-row branch. It failed
// with:
//
//	403 PERMISSION_DENIED — "Method doesn't allow unregistered callers ...
//	Please use API Key or other form of API consumer identity to call this API."
//
// after 8 seconds. That body did not come from the test server. proxy.New()
// initialises openAIURL/anthropicURL/googleURL to the REAL endpoints
// (proxy.go: openAIChatURL, anthropicMessageURL, googleGenerativeLanguageURL), and
// the compression helper overrode only the first two. The 500 made the fallback
// router walk anthropic → openai → google, and the third attempt dialled
// generativelanguage.googleapis.com from the test process.
//
// ⚠ WHY IT MATTERS EVEN THOUGH EVERY SHIPPED TEST WAS GREEN. The escape is latent,
// not absent: it needs only an upstream that fails, and the first test to write one
// found it immediately. A suite that can reach a third party is not hermetic — its
// verdict depends on DNS, on a network, and on a stranger's rate limiter, and in CI
// that reads as a flake rather than as a bug. It is also an unannounced outbound
// request from a build machine.
//
// ⚠ WHAT THIS GUARD DOES NOT COVER, SAID PLAINLY: it checks the proxies the
// COMPRESSION helpers build, because those are the ones this change owns. Other
// helpers in this package construct a Proxy through New() and override a subset of
// URLs the same way — the same latent escape, not fixed here and not pretended
// away. See the queue note for the census.

// providerURLs is every base URL a Proxy could dial, keyed by provider. Empty
// means "not configured" — nothing to dial — and is not an escape.
func providerURLs(p *Proxy) map[string]string {
	return map[string]string{
		"openai":    p.openAIURL,
		"anthropic": p.anthropicURL,
		"google":    p.googleURL,
		"mistral":   p.mistralURL,
		"groq":      p.groqURL,
		"vllm":      p.vllmURL,
		"bedrock":   p.bedrockURL,
	}
}

// pointEveryProviderAt redirects every provider — not just the one a test means to
// exercise — at the given test server, so no fallback hop can leave the machine.
func pointEveryProviderAt(p *Proxy, testServerURL string) {
	p.openAIURL = testServerURL
	p.anthropicURL = testServerURL
	p.googleURL = testServerURL
	p.mistralURL = testServerURL
	p.groqURL = testServerURL
	p.vllmURL = testServerURL
	p.bedrockURL = testServerURL
}

// isLoopback reports whether a configured base URL resolves to this machine.
// A URL that will not parse, or whose host is not a loopback literal, counts as
// off-machine: the failure direction is "assume it escapes".
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// assertHermetic fails naming the provider, so a red says which URL escaped rather
// than that something, somewhere, is wrong.
func assertHermetic(t *testing.T, p *Proxy) {
	t.Helper()
	checked := 0
	for name, raw := range providerURLs(p) {
		if raw == "" {
			continue
		}
		checked++
		if !isLoopback(raw) {
			t.Errorf("provider %q would be dialled at %q — a fallback hop from this test leaves the machine", name, raw)
		}
	}
	// A sweep over an empty set is green. Without this, a Proxy whose URLs were
	// all somehow blank would "pass" hermetically while proving nothing.
	if checked == 0 {
		t.Fatal("no provider URL was configured at all — this assertion read nothing and cannot have passed anything")
	}
}

// THE GUARD. Both compression helpers must produce a proxy with no route off the
// machine, whatever the fallback router decides to try.
func TestCompressionHelpers_DialNothingOffTheMachine(t *testing.T) {
	t.Run("succeeding upstream", func(t *testing.T) {
		p, _, _ := newCompressionProxyWithUpstream(t, testHermeticWorkspace(), measuredUsageResponse)
		assertHermetic(t, p)
	})
	t.Run("failing upstream", func(t *testing.T) {
		p, _, _ := newFailingUpstreamMeasuredProxy(t, testHermeticWorkspace())
		assertHermetic(t, p)
	})
}

// THE CONTROL FOR THE GUARD ABOVE, and it is not optional: isLoopback returning
// true for everything would make assertHermetic unfalsifiable, and the guard would
// pass on the very configuration it exists to catch. These are the exact defaults
// proxy.New() ships.
func TestHermeticity_TheDetectorRecognisesTheRealEndpoints(t *testing.T) {
	for _, escape := range []string{
		openAIChatURL,
		anthropicMessageURL,
		googleGenerativeLanguageURL,
		"https://api.mistral.ai",
		"https://api.groq.com",
	} {
		if isLoopback(escape) {
			t.Errorf("isLoopback(%q) = true — the hermeticity guard cannot see a real endpoint", escape)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:1", "http://[::1]:99"} {
		if !isLoopback(ok) {
			t.Errorf("isLoopback(%q) = false — the guard would fail every hermetic test", ok)
		}
	}
}
