package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/workspace"
)

// WHERE THE COMPRESSION GATE IS NOT.
//
// compression_gate_test.go proves the gate decides correctly for the requests
// that REACH it, and compression_measure_test.go proves the row written about
// those requests describes the bytes the provider got. Neither can see the
// question this file asks: WHICH REQUESTS REACH THE GATE AT ALL.
//
// The gate is consulted at exactly one point in proxy.serve — proxy.go#serve,
// just below the streaming branch's `return` and just below the cache-hit
// branch's. Two large classes of request are therefore decided before the gate
// exists, and a third receives a rewrite-derived answer without ever being
// gated. All three are MEASURED here rather than reasoned about, because the
// enumeration they falsify (captureCompression's "the population is exactly")
// is prose and prose cannot be run.
//
// ⚠ EVERY TEST HERE PASSES ON A PRISTINE TREE. They pin behaviour that exists,
// so none of them could be red-first and the POSITIVE CONTROLS are the evidence
// that they are not vacuous (w61-population-controls-6b2f.py). Each one
// therefore carries a FLOOR: an assertion that the request under test actually
// happened, so an absence measured here is a measured absence and not a dead
// proxy, an unwired sink or a dispatch that never left the helper.
//
// ⚠ NOTHING HERE IS FIXED, AND THAT IS DELIBERATE — see the note on each test.
// The cache key is the pooling hit rate (W2.1 measured it); compressing the
// streaming path is a product change on the seam that also skips routing. Both
// are decisions, and this file's job is to make them decisions somebody can see.

// streamAwareUpstream answers SSE to a streaming request and JSON to a
// non-streaming one, so ONE proxy can serve both shapes of the same prompt.
// That is what makes "streaming" the variable under test rather than "this
// proxy happens not to compress".
func newStreamAwareMeasuredProxy(t *testing.T, ws workspace.Workspace) (*Proxy, *capturingUpstream, *recordingCompressionSink) {
	t.Helper()
	up := &capturingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.add(b)
		if streamRequested(b) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n"))
			_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, measuredUsageResponse)
	}))
	t.Cleanup(srv.Close)

	p, _, sink := newMeasuredProxy(t, ws, measuredUsageResponse)
	pointEveryProviderAt(p, srv.URL)
	return p, up, sink
}

// dispatchCompressStreaming is dispatchCompress with "stream": true.
func dispatchCompressStreaming(t *testing.T, p *Proxy, wsID, prompt string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "claude-haiku-4-5",
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   true,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("streaming status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	return w
}

// theRewrite is the compressor's output for a prompt, plus the assertion that it
// actually differs from the input. Every test below distinguishes "the original
// reached the provider" from "the rewrite did", and that distinction is empty if
// the two strings are equal — which is what a compressor blinded to this prompt
// would produce.
func theRewrite(t *testing.T, prompt string) string {
	t.Helper()
	got := compressor.New().Compress(context.Background(), prompt).CompressedPrompt
	if got == prompt {
		t.Fatalf("the compressor does not rewrite %q at all — every assertion in this file that tells the rewrite from the original would be vacuous", prompt)
	}
	return got
}

// ⚠ THE PER-REQUEST OPT-IN IS NOT PER-REQUEST IN ITS EFFECT.
//
// X-Talyvor-Compress: true is documented as the caller's consent for THIS
// request (compression_gate.go, and TestUpstream_OptInWithoutHeaderSendsThePrompt
// Unchanged pins the no-header case at the wire). But the response produced from
// the REWRITTEN prompt is cached under the key of the UNREWRITTEN one —
// proxy.go#serve builds cachePrompt as wsID+":"+prompt BEFORE the rewrite, and
// nothing in the key records that the rewriter ran. So one opted-in request
// answers every later identical request in that workspace, header or no header.
//
// WHY IT IS NOT A CURIOSITY: the rewriter is not lossless. On this file's own
// fixture it turns "when to use 'in order to' versus 'to'" into "when to use
// 'to' versus 'to'" — the quoted phrase the question is ABOUT is substituted
// away. A caller who never opted in receives the answer to that question.
//
// ⚠ NOT FIXED — IT IS A DECISION AND THE DECISION IS ABOUT MONEY. Putting the
// compression state into the cache key splits the keyspace in two and moves the
// hit rate, which W2.1 measured and which the pooling economics rest on. The
// alternative — not caching a rewrite-derived response — gives compressed
// workspaces no cache at all. Pinned so the choice is visible, not made silently.
func TestCompressionCache_AnOptedInRewriteAnswersARequestThatDidNotOptIn(t *testing.T) {
	p, up := newCompressionProxy(t, workspace.Workspace{
		ID: "ws-leak", Name: "per-request", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionOptIn,
	})
	rewrite := theRewrite(t, compressiblePrompt)

	// FLOOR 1: the opted-in request must genuinely have reached the provider
	// carrying the REWRITE. Without this the "leak" below is just a proxy that
	// never called anyone.
	dispatchCompress(t, p, "ws-leak", compressiblePrompt, map[string]string{"X-Talyvor-Compress": "true"})
	if len(up.bodies) != 1 {
		t.Fatalf("floor: the opted-in request made %d upstream calls, want 1", len(up.bodies))
	}
	if got := up.lastPrompt(t); got != rewrite {
		t.Fatalf("floor: the opted-in request did not send the rewrite\n got  %q\n want %q", got, rewrite)
	}

	// THE LEAK. Same prompt, no header. The gate says "do not rewrite"; the cache
	// answers before the gate is ever consulted.
	dispatchCompress(t, p, "ws-leak", compressiblePrompt, nil)
	if len(up.bodies) != 1 {
		t.Fatalf("the un-opted request DID reach the provider (%d calls) — the leak this test pins is gone, and the comment above plus the cache-key note in proxy.go#serve are now stale", len(up.bodies))
	}

	// FLOOR 2 — THE ANTI-VACUITY CONTROL, AND IT IS THE IMPORTANT ONE. A proxy
	// that had simply stopped forwarding would satisfy the assertion above. A
	// DIFFERENT prompt, still with no header, must reach the provider AND arrive
	// unrewritten. That is what proves the cache swallowed the request and that
	// the gate is still off for it.
	const otherPrompt = "Please  summarise in order to keep it short as well as  clear."
	_ = theRewrite(t, otherPrompt) // the fixture must be compressible, or "unrewritten" says nothing
	dispatchCompress(t, p, "ws-leak", otherPrompt, nil)
	if len(up.bodies) != 2 {
		t.Fatalf("floor: a NEW un-opted prompt made %d total upstream calls, want 2 — this proxy is not forwarding at all and the assertion above passed for the wrong reason", len(up.bodies))
	}
	if got := up.lastPrompt(t); got != otherPrompt {
		t.Fatalf("floor: the new un-opted prompt was rewritten, so the gate is not off\n got  %q\n want %q", got, otherPrompt)
	}
}

// ⚠ A CACHE HIT IS A GATED REQUEST THAT IS NOT IN THE DENOMINATOR.
//
// captureCompression's docstring enumerates the population and says it is
// EXACTLY "gated requests THAT PRODUCED A SPEND ROW, minus observational sheds",
// naming the 200/output-blocked/LoggingNone exclusions and pinning two of them.
// A cache hit is none of those: it is a 200, it is not output-blocked, its
// workspace logs, and it is not shed. It returns from proxy.go#serve above the
// gate, so the rewriter never runs and no row is written.
//
// WHY IT MATTERS FOR THE NUMBER: cache hits are the requests that cost the
// least, so excluding them biases `requests` towards the expensive tail. Anyone
// dividing bytes_removed by requests is dividing by the upstream-served subset,
// which is a different denominator from the one the prose describes.
func TestCompressionMeasure_ACacheHitIsNotInTheDenominator(t *testing.T) {
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-hit", Name: "always", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	// FLOOR: the FIRST request must be measured, and measured about this prompt.
	// An unwired sink would otherwise make "the second was not measured" true for
	// a reason that has nothing to do with the cache.
	dispatchCompress(t, p, "ws-hit", compressiblePrompt, nil)
	first := sink.all()
	if len(first) != 1 {
		t.Fatalf("floor: the first gated request wrote %d rows, want 1 — the sink is not wired and this test would pass empty", len(first))
	}
	if first[0].OriginalBytes != len(compressiblePrompt) {
		t.Fatalf("floor: the row does not describe this prompt: original_bytes=%d, want %d", first[0].OriginalBytes, len(compressiblePrompt))
	}
	if len(up.bodies) != 1 {
		t.Fatalf("floor: the first request made %d upstream calls, want 1", len(up.bodies))
	}

	// The second, identical, gated request. It is served from cache.
	dispatchCompress(t, p, "ws-hit", compressiblePrompt, nil)
	if len(up.bodies) != 1 {
		t.Fatalf("floor: the second request reached the provider (%d calls) — it was not a cache hit, so this test is measuring something else entirely", len(up.bodies))
	}
	if rows := sink.all(); len(rows) != 1 {
		t.Fatalf("two gated requests produced %d measurement rows, want 1 — a cache hit is now in the denominator and captureCompression's population enumeration needs updating", len(rows))
	}
}

// ⚠ A STREAMING REQUEST IS NEITHER COMPRESSED NOR MEASURED, IN A WORKSPACE WHOSE
// POLICY IS `always`.
//
// proxy.go#serve's streaming branch returns before the gate exists, handing the
// caller's ORIGINAL body to StreamHandler. So `always` does not mean "every
// request": it means every non-streaming request, which is what
// workspace.CompressionAlways's own docstring says — and which the measurement
// layer's population enumeration does not.
//
// WHY IT IS THE LOAD-BEARING EXCLUSION RATHER THAN A CORNER: streaming is the
// shape coding agents use, and poolsafety.Corpus() — this repo's model of real
// agent traffic — is the corpus the rewriter was measured to modify 8 of 8 times.
// The traffic the feature was justified on is the traffic it does not run on, and
// `requests` never mentions it.
//
// ⚠ NOT FIXED. Compressing the streaming path means rewriting the body the
// StreamHandler forwards verbatim, on the same seam that deliberately skips
// routing; and the measurement would then need a post-stream hook that does not
// exist. A product decision, not a tidy-up.
func TestCompressionGate_AStreamingRequestIsNeitherCompressedNorMeasured(t *testing.T) {
	ws := workspace.Workspace{
		ID: "ws-stream", Name: "always", Active: true,
		LoggingPolicy:     workspace.LoggingMetadata,
		CompressionPolicy: workspace.CompressionAlways,
	}
	p, up, sink := newStreamAwareMeasuredProxy(t, ws)
	rewrite := theRewrite(t, compressiblePrompt)

	// FLOOR — THE DISCRIMINATOR. The SAME proxy, the SAME workspace and a
	// DIFFERENT prompt, sent NON-streaming, must be rewritten and measured. This
	// is what makes "streaming" the variable: without it, a proxy whose
	// compressor was blinded entirely would satisfy every assertion below.
	const controlPrompt = "Please  rewrite this in order to shorten it as well as  clarify."
	controlRewrite := theRewrite(t, controlPrompt)
	dispatchCompress(t, p, "ws-stream", controlPrompt, nil)
	if got := up.lastPrompt(t); got != controlRewrite {
		t.Fatalf("floor: a NON-streaming request in this same `always` workspace was not rewritten\n got  %q\n want %q", got, controlRewrite)
	}
	if rows := sink.all(); len(rows) != 1 {
		t.Fatalf("floor: the non-streaming control wrote %d measurement rows, want 1 — the sink is unwired and the zero below would mean nothing", len(rows))
	}
	upstreamBefore := len(up.bodies)

	// The streaming request.
	dispatchCompressStreaming(t, p, "ws-stream", compressiblePrompt)
	if len(up.bodies) != upstreamBefore+1 {
		t.Fatalf("floor: the streaming request made no upstream call (%d → %d) — nothing below was exercised", upstreamBefore, len(up.bodies))
	}
	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Fatalf("a streaming request in an `always` workspace is now compressed\n got  %q\n want the caller's own bytes %q\n(the rewrite would have been %q) — the comment above and workspace.CompressionAlways's docstring both need updating", got, compressiblePrompt, rewrite)
	}
	if rows := sink.all(); len(rows) != 1 {
		t.Fatalf("the streaming request wrote a measurement row (%d rows total, want the control's 1) — captureCompression's population enumeration needs updating", len(rows))
	}
}
