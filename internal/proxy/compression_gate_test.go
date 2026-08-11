package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// THESE TESTS ASSERT ON WHAT IS SENT UPSTREAM, NOT ON THE COMPRESSOR FUNCTION.
//
// internal/compressor's own tests prove the rewriter rewrites. That is not the
// question the gate answers. The question is which bytes the PROVIDER receives,
// because that is the only place the rewrite can change an answer or a bill — and
// the rewrite reaches it through proxy.serve → rebuildBody, not through Compress.
// A unit test of Compress is a must-stay-green companion here, never the catcher.
//
// The upstream prompt is read back out of the captured request body: rebuildBody
// re-writes messages as a single user message whose content is the prompt that was
// actually forwarded (rebuildBody, proxy.go:1834).

// capturingUpstream records every request body the "provider" received.
type capturingUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *capturingUpstream) add(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

// lastPrompt returns the user-message content of the most recent upstream call.
func (c *capturingUpstream) lastPrompt(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream received no request at all — the assertion below would be vacuous")
	}
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(c.bodies[len(c.bodies)-1], &m); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, c.bodies[len(c.bodies)-1])
	}
	if len(m.Messages) == 0 {
		t.Fatalf("upstream body carried no messages: %s", c.bodies[len(c.bodies)-1])
	}
	return m.Messages[len(m.Messages)-1].Content
}

// newCompressionProxy wires a proxy whose upstream is captured, with ONE
// registered workspace carrying the given compression policy. The upstream
// REPORTS usage, so the gate tests below exercise the ordinary billed path.
func newCompressionProxy(t *testing.T, ws workspace.Workspace) (*Proxy, *capturingUpstream) {
	t.Helper()
	p, up, _ := newCompressionProxyWithUpstream(t, ws,
		`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
	return p, up
}

// newCompressionProxyWithUpstream is the same wiring with the provider's response
// body under the caller's control, and the spend sink returned. The billing test
// needs an upstream that reports NO usage, because that is the only path where
// the len/4 estimate IS the charge.
func newCompressionProxyWithUpstream(t *testing.T, ws workspace.Workspace, respBody string) (*Proxy, *capturingUpstream, *recordingAlertSink) {
	t.Helper()
	up := &capturingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.add(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	p.anthropicURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, up, sink
}

// compressiblePrompt is rewritten by the shipped compressor in FOUR independent
// ways (filler deletion, phrase substitution, space-run collapse, blank-line
// removal), so no single technique going quiet can make these tests vacuous.
const compressiblePrompt = "Please  explain when to use 'in order to' versus 'to' as well as  why."

func dispatchCompress(t *testing.T, p *Proxy, wsID, prompt string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "claude-haiku-4-5",
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}

// THE RED-FIRST ASSERTION. A workspace that has set NO compression policy — every
// workspace that exists today — must have its prompt reach the provider BYTE-FOR-BYTE.
// Before the gate this failed: compressor.Compress ran unconditionally at
// proxy.go:1242 and rebuildBody forwarded the rewrite.
func TestUpstream_DefaultWorkspaceSendsThePromptUnchanged(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-default", Name: "no policy set", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
	})
	dispatchCompress(t, p, "ws-default", compressiblePrompt, nil)

	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Errorf("upstream prompt was REWRITTEN for a workspace with no compression policy\n got  %q\n want %q", got, compressiblePrompt)
	}
}

// The MUST-STAY-GREEN companion: the seam still compresses when a workspace asks
// for it. Without this, blinding the gate to "never compress" would be invisible
// and the test above would pass for the wrong reason.
func TestUpstream_CompressionAlwaysSendsTheRewrite(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-always", Name: "opted in", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	})
	dispatchCompress(t, p, "ws-always", compressiblePrompt, nil)

	got := up.lastPrompt(t)
	if got == compressiblePrompt {
		t.Fatalf("policy=always must send the REWRITE, not the original: %q", got)
	}
	// Pin what the rewrite actually is, so "modified somehow" cannot stand in for
	// "modified by the compressor".
	want := compressor.New().Compress(context.Background(), compressiblePrompt).CompressedPrompt
	if got != want {
		t.Errorf("upstream prompt is not the compressor's output\n got  %q\n want %q", got, want)
	}
}

// opt_in without the header: unchanged. This is the case a workspace lands in
// when it wants per-request control, and it is where a gate that reads the policy
// but ignores the header would leak the rewrite.
func TestUpstream_OptInWithoutHeaderSendsThePromptUnchanged(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-optin", Name: "per-request", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionOptIn,
	})
	dispatchCompress(t, p, "ws-optin", compressiblePrompt, nil)

	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Errorf("opt_in with no header must send the original\n got  %q\n want %q", got, compressiblePrompt)
	}
}

// opt_in WITH the header: the rewrite is sent. Mirrors X-Talyvor-Distill.
func TestUpstream_OptInWithHeaderSendsTheRewrite(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-optin-hdr", Name: "per-request", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionOptIn,
	})
	dispatchCompress(t, p, "ws-optin-hdr", compressiblePrompt, map[string]string{"X-Talyvor-Compress": "true"})

	if got := up.lastPrompt(t); got == compressiblePrompt {
		t.Errorf("opt_in + X-Talyvor-Compress: true must send the rewrite; got the original %q", got)
	}
}

// The header ALONE cannot turn compression on. A disabled workspace stays off no
// matter what the client sends — otherwise "disabled" would be advisory.
func TestUpstream_HeaderCannotOverrideDisabled(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-off", Name: "explicitly off", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionDisabled,
	})
	dispatchCompress(t, p, "ws-off", compressiblePrompt, map[string]string{"X-Talyvor-Compress": "true"})

	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Errorf("a disabled workspace must ignore the header\n got  %q\n want %q", got, compressiblePrompt)
	}
}

// An UNREGISTERED workspace (and, by the same path, a proxy with no workspace
// manager at all) must not compress: the gate's failure direction is OFF.
func TestUpstream_UnregisteredWorkspaceDoesNotCompress(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-known", Name: "registered", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	})
	// A different workspace id, never registered. Registration of ws-known with
	// policy=always is what makes this discriminating: the gate must answer on
	// THIS request's workspace, not on "some workspace opted in".
	dispatchCompress(t, p, "ws-unknown", compressiblePrompt, nil)

	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Errorf("an unregistered workspace must not be compressed\n got  %q\n want %q", got, compressiblePrompt)
	}
}

// A PROXY WITH NO WORKSPACE MANAGER AT ALL. This is the one branch of the gate
// that no wire test above can reach — every one of them registers a workspace —
// and it is load-bearing outside the product: benchmarks/bench_test.go wires
// proxy.New with a nil manager, so BenchmarkFullProxyStack's "no longer includes
// compression" note is a claim about THIS branch. Asserted directly rather than
// left to be inferred from a nil check being visible in the source.
func TestShouldCompress_NilWorkspaceManagerIsOff(t *testing.T) {
	p := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", nil)
	req.Header.Set("X-Talyvor-Compress", "true")
	if p.shouldCompress(req, "any-ws") {
		t.Error("a proxy with no workspace manager must not rewrite prompts — there is nothing that could have consented")
	}
	if p.shouldCompress(nil, "any-ws") {
		t.Error("a nil request must not rewrite prompts either")
	}
}

// THE CODE CASE, ASSERTED THROUGH THE WIRE. An unfenced snippet's indentation is
// what the space-run collapse destroys, and the corpus that models real agent
// traffic (poolsafety.Corpus()) is 8/8 affected — see internal/compressor's
// reach test. Here it is pinned where it matters: the provider's bytes.
func TestUpstream_UnfencedCodeIndentationSurvivesByDefault(t *testing.T) {
	const snippet = "Fix this:\ndef f(vol):\n    if vol.ndim != 3:\n        raise ValueError('rank')\n    return vol.sum()\n"
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-code", Name: "agent traffic", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
	})
	dispatchCompress(t, p, "ws-code", snippet, nil)

	got := up.lastPrompt(t)
	if got != snippet {
		t.Errorf("unfenced code reached the provider re-indented\n got  %q\n want %q", got, snippet)
	}
	// And name the specific loss, so a future partial regression still reads as
	// this defect rather than as a generic byte diff.
	if !strings.Contains(got, "        raise ValueError('rank')") {
		t.Errorf("the two nesting levels collapsed into one: %q", got)
	}
}
