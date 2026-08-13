package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/backpressure"
	"github.com/talyvor/lens/internal/compressmeasure"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// WHAT THIS FILE ASSERTS, AND WHY IT IS NOT THE STORE'S TEST.
//
// compressmeasure's own tests prove a row round-trips. That is not the question.
// The question is whether the row the SERVE PATH writes describes the bytes the
// provider actually received — because the whole point of step 2 is that the
// saving become observable BEFORE anyone tries to make it bigger, and an
// observation taken from the wrong string is worse than none.
//
// So every assertion here goes through proxy.serve and cross-checks the recorded
// byte counts against the CAPTURED UPSTREAM BODY where it can. A test that only
// consulted the sink would be green against a writer that measured the prompt it
// never sent.

// recordingCompressionSink captures every measurement the serve path writes.
type recordingCompressionSink struct {
	mu   sync.Mutex
	rows []compressmeasure.Measurement
	err  error
}

func (s *recordingCompressionSink) Record(_ context.Context, m compressmeasure.Measurement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, m)
	return s.err
}

func (s *recordingCompressionSink) all() []compressmeasure.Measurement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]compressmeasure.Measurement(nil), s.rows...)
}

// only returns THE single recorded row, failing loudly on 0 or 2+. Every
// assertion below goes through it, so a writer that fired twice (or not at all)
// cannot read as a pass.
func (s *recordingCompressionSink) only(t *testing.T) compressmeasure.Measurement {
	t.Helper()
	rows := s.all()
	if len(rows) != 1 {
		t.Fatalf("recorded %d measurements, want exactly 1 — the assertions below would be vacuous", len(rows))
	}
	return rows[0]
}

// newMeasuredProxy is newCompressionProxyWithUpstream plus the measurement sink.
// respBody is under the caller's control because the ESTIMATED path (no provider
// usage block) is the one where the saving provably reaches no bill.
func newMeasuredProxy(t *testing.T, ws workspace.Workspace, respBody string) (*Proxy, *capturingUpstream, *recordingCompressionSink) {
	t.Helper()
	p, up, _ := newCompressionProxyWithUpstream(t, ws, respBody)
	sink := &recordingCompressionSink{}
	p.SetCompressionSink(sink)
	return p, up, sink
}

// newFailingUpstreamMeasuredProxy wires the same proxy against a provider that
// answers 500 to everything, so the serve path never reaches its spend-row branch.
// The upstream is still CAPTURING: the assertion that matters is about a request
// whose rewritten prompt genuinely left the gateway.
//
// ⚠ THE ANTHROPIC FALLBACK CHAIN IS EMPTIED, AND THAT IS NOT A SPEED TRICK — IT IS
// WHAT KEEPS THIS TEST OFF THE PUBLIC INTERNET. Measured, before the chain was
// emptied: a 500 from the test server made the router try anthropic → openai →
// GOOGLE, and this package's helpers override only openAIURL and anthropicURL, so
// the third attempt dialled generativelanguage.googleapis.com for real and came
// back "PERMISSION_DENIED ... unregistered callers" after 8 seconds. The assertion
// then failed for a reason that had nothing to do with compression. An empty chain
// makes the failure terminal at the first provider: one attempt, no network, ~0s.
// hermetic_upstreams_test.go carries the standing guard for the same hazard.
func newFailingUpstreamMeasuredProxy(t *testing.T, ws workspace.Workspace) (*Proxy, *capturingUpstream, *recordingCompressionSink) {
	t.Helper()
	up := &capturingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		up.add(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream is down"}}`)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	fb := fallback.New()
	fb.SetChain("anthropic", nil) // terminal on the first failure; see the note above
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fb, nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	pointEveryProviderAt(p, srv.URL)
	p.setAlertSink(&recordingAlertSink{})
	sink := &recordingCompressionSink{}
	p.SetCompressionSink(sink)
	return p, up, sink
}

// dispatchCompressExpectingFailure is dispatchCompress without its 200 assertion —
// this file needs a request that FAILS, and the shared helper t.Fatalf's on
// anything else.
func dispatchCompressExpectingFailure(t *testing.T, p *Proxy, wsID, prompt string) *httptest.ResponseRecorder {
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
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	return w
}

const measuredUsageResponse = `{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":2}}`

// THE DENOMINATOR. A gated request that saved NOTHING must still be recorded.
// This is the assertion that separates an honest measurement from a highlight
// reel: the measured truth about this rewriter is that it modifies 0 of 308
// committed corpus prompts, and a table holding only hits would render that as
// indistinguishable from a rewriter that never ran.
func TestMeasure_AGatedRequestThatSavesNothingIsStillRecorded(t *testing.T) {
	// "hello world" carries no filler, no phrase pattern, no space run and no
	// fence — the rewriter cannot touch it.
	const inert = "hello world"
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-inert", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-inert", inert, nil)

	if got := up.lastPrompt(t); got != inert {
		t.Fatalf("premise: upstream got %q, want the prompt unchanged", got)
	}
	m := sink.only(t)
	if m.Modified {
		t.Errorf("modified = true for a prompt the rewriter cannot change")
	}
	if m.OriginalBytes != len(inert) || m.SentBytes != len(inert) {
		t.Errorf("bytes = (%d, %d), want (%d, %d)", m.OriginalBytes, m.SentBytes, len(inert), len(inert))
	}
	if m.WorkspaceID != "ws-inert" {
		t.Errorf("workspace_id = %q, want ws-inert", m.WorkspaceID)
	}
	if m.RequestID == "" {
		t.Errorf("request_id is empty — the PK that stops a retry double-counting the denominator")
	}
}

// THE GATE IS THE POPULATION BOUNDARY. A workspace with no compression policy —
// every workspace that exists — has no rewrite to observe, so it writes no row.
// Recording those would put hundreds of thousands of "saved 0 bytes" rows into
// the denominator and drown the population the reader is about.
func TestMeasure_TheGateClosedRecordsNothing(t *testing.T) {
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-default", Name: "no policy set", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-default", compressiblePrompt, nil)

	if got := up.lastPrompt(t); got != compressiblePrompt {
		t.Fatalf("premise: the default workspace must send the prompt unchanged; got %q", got)
	}
	if rows := sink.all(); len(rows) != 0 {
		t.Errorf("recorded %d measurements with the gate closed, want 0: %+v", len(rows), rows)
	}
}

// The recorded byte counts must describe THE WIRE. sent_bytes is cross-checked
// against the prompt the captured upstream actually received, so a writer that
// measured the original twice cannot pass.
func TestMeasure_SentBytesAreTheBytesTheProviderReceived(t *testing.T) {
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-on", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-on", compressiblePrompt, nil)

	upstream := up.lastPrompt(t)
	if upstream == compressiblePrompt {
		t.Fatalf("premise: the rewriter must have changed this prompt; upstream got it verbatim")
	}
	m := sink.only(t)
	if !m.Modified {
		t.Errorf("modified = false for a prompt the provider received rewritten")
	}
	if m.SentBytes != len(upstream) {
		t.Errorf("sent_bytes = %d, but the provider received %d bytes (%q)", m.SentBytes, len(upstream), upstream)
	}
	if m.OriginalBytes != len(compressiblePrompt) {
		t.Errorf("original_bytes = %d, want %d", m.OriginalBytes, len(compressiblePrompt))
	}
	if m.SentBytes >= m.OriginalBytes {
		t.Errorf("sent_bytes %d >= original_bytes %d — nothing was removed", m.SentBytes, m.OriginalBytes)
	}
}

// THE CATCHER. A percentage cannot see this change and a string comparison can.
//
// A blank line removed inside a fenced Python block alters the bytes the provider
// receives while the rewriter's own SavingsPct reads exactly 0.00%, because
// savings are computed on len/4 integer division (compressor.
// TestSavings_ZeroDoesNotMeanUntouched pins the arithmetic). If `modified` is ever
// re-derived from savingsPct — the obvious, tempting implementation, and the exact
// shape of the tombstoned token_events.savings_pct — this test is what fails.
//
// It is positive-controlled in w61-measure-controls-6c3f.py by rewriting the
// writer's Modified field to `savingsPct > 0` and confirming this test, and only
// this one, goes red.
func TestMeasure_ModifiedIsAStringComparisonNotAPercentage(t *testing.T) {
	// 26 bytes in, 25 out: len/4 is 6 either side, so the percentage is 0.00.
	const fenced = "```python\nx = 1\n\ny = 2\n```"
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-fence", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-fence", fenced, nil)

	upstream := up.lastPrompt(t)
	if upstream == fenced {
		t.Fatalf("premise: this input must reach the provider changed; it did not")
	}
	if len(fenced)/4 != len(upstream)/4 {
		t.Fatalf("premise: this case must be invisible to len/4 (%d vs %d) or it is not the trap",
			len(fenced)/4, len(upstream)/4)
	}
	m := sink.only(t)
	if !m.Modified {
		t.Errorf("modified = false for a prompt whose bytes changed — this is the savings_pct defect again")
	}
	if m.OriginalBytes == m.SentBytes {
		t.Errorf("original_bytes == sent_bytes == %d, but the wire disagrees", m.SentBytes)
	}
}

// THE HALF THAT KEEPS THE NUMBER HONEST. With no provider usage block the spend
// row is written from len(ORIGINAL)/4, so however many bytes the rewriter removed,
// the customer is billed as if it removed none. The measurement must carry that,
// or a reader could report bytes removed as a saving nobody received.
func TestMeasure_OnTheEstimatedPathTheBillFollowsTheORIGINAL(t *testing.T) {
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-est", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, `{"content":[{"type":"text","text":"ok"}]}`) // no usage block ⇒ estimated

	dispatchCompress(t, p, "ws-est", compressiblePrompt, nil)

	upstream := up.lastPrompt(t)
	if upstream == compressiblePrompt {
		t.Fatalf("premise: the rewriter must have changed this prompt")
	}
	m := sink.only(t)
	if !m.CostEstimated {
		t.Fatalf("cost_estimated = false, but the upstream reported no usage — the premise of this test is gone")
	}
	if want := len(compressiblePrompt) / 4; m.BilledInputTokens != want {
		t.Errorf("billed_input_tokens = %d, want %d = len(ORIGINAL)/4", m.BilledInputTokens, want)
	}
	if m.BilledInputTokens == len(upstream)/4 && len(upstream)/4 != len(compressiblePrompt)/4 {
		t.Errorf("billed on the SENT length — this test's whole premise about the estimate is stale")
	}
}

// The provider's own count is what the bill follows when it reports one, so
// cost_estimated must distinguish the two paths rather than being constant.
// Without this, EstimatedPathRequests could be structurally equal to Requests and
// the reader would report a true-looking number that means nothing.
func TestMeasure_TheProviderReportedPathIsRecordedAsNotEstimated(t *testing.T) {
	p, _, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-rep", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-rep", compressiblePrompt, nil)

	m := sink.only(t)
	if m.CostEstimated {
		t.Errorf("cost_estimated = true although the provider reported usage")
	}
	if m.BilledInputTokens != 10 {
		t.Errorf("billed_input_tokens = %d, want the provider's reported 10", m.BilledInputTokens)
	}
}

// LoggingNone is the privacy escape hatch: every DB and NATS sink is bypassed.
// The measurement is metadata, not content — but it is still a per-request row
// tied to a workspace, and the escape hatch does not have exceptions.
func TestMeasure_LoggingNoneRecordsNothing(t *testing.T) {
	p, _, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-none", Name: "privacy hatch", Active: true,
		CompressionPolicy: workspace.CompressionAlways, LoggingPolicy: workspace.LoggingNone,
	}, measuredUsageResponse)

	dispatchCompress(t, p, "ws-none", compressiblePrompt, nil)

	if rows := sink.all(); len(rows) != 0 {
		t.Errorf("recorded %d measurements for a LoggingNone workspace, want 0: %+v", len(rows), rows)
	}
}

// THE POPULATION IS NARROWER THAN "GATED", PART 1: AN UPSTREAM FAILURE.
//
// The prompt was rewritten and sent — the provider received the compressed bytes —
// and then answered badly. No spend row is written for that request, and the
// measurement, which lives inside the spend-row branch, is absent too. This is the
// intended design (every recorded row carries a real billed_input_tokens), but it
// means `requests` is the BILLED subset of gated requests, not all of them.
//
// ⚠ THIS IS AN ABSENCE TEST AND SO CANNOT BE RED-FIRST. Its positive control is
// C17 of w61-measure-controls-4f2b.py: it hoists captureCompression out of the
// spend-row branch to just after the rewrite, where it fires for every gated
// request, and this test is the one that goes red while
// TestMeasure_TheGateClosedRecordsNothing holds.
//
// ⚠ THE CITATION THIS REPLACES NAMED A FILE THAT DID NOT EXIST. It read
// "w61-measure-controls-e62d.py" — no such harness was ever written, here or in
// ~/talyvor-queue. For a week this comment described a control that had never
// been run, and the test it vouched for was one of THREE absence tests in this
// file with no control at all (the others: ASaturatedWriterBoundSilentlyDropsTheRow,
// ASinkErrorDoesNotAffectTheServedResponse — C18 and C19). An absence assertion
// passes for free when the instrument is dead, so a cited-but-absent control is
// worse than an admitted gap: it reads as evidence.
func TestMeasure_AnUpstreamFailureRecordsNothing(t *testing.T) {
	p, up, sink := newFailingUpstreamMeasuredProxy(t, workspace.Workspace{
		ID: "ws-fail", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	})

	w := dispatchCompressExpectingFailure(t, p, "ws-fail", compressiblePrompt)

	if w.Code == http.StatusOK {
		t.Fatalf("premise: the upstream must have failed this request; got %d", w.Code)
	}
	if up.lastPrompt(t) == compressiblePrompt {
		t.Fatal("premise: the gate was open, so the provider must have received the REWRITTEN prompt — " +
			"this test is about a request whose rewrite genuinely went out")
	}
	if rows := sink.all(); len(rows) != 0 {
		t.Errorf("recorded %d measurements for a request that produced no spend row, want 0: %+v", len(rows), rows)
	}
}

// THE POPULATION IS NARROWER THAN "GATED", PART 2: THE OBSERVATIONAL SHED.
//
// captureCompression shares the obsLimiter bound with the other post-flush
// captures. When that bound is saturated the row is DROPPED — the request serves
// normally, the provider still received the rewritten bytes, and the denominator
// silently loses one. An operator reading `requests` under load is reading a
// number that shrank for a reason the JSON does not carry.
//
// The limiter is saturated by taking its only slot and never releasing it, so the
// shed is deterministic rather than a race.
//
// ⚠ ALSO AN ABSENCE TEST. Positive control C18 of w61-measure-controls-4f2b.py
// deletes captureCompression's obsLimiter block; the row is then written despite
// the saturated bound and this test goes red while the ordinary recorded request
// holds green.
func TestMeasure_ASaturatedWriterBoundSilentlyDropsTheRow(t *testing.T) {
	p, up, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-shed", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	p.obsLimiter = backpressure.New(1)
	if !p.obsLimiter.TryAcquire() {
		t.Fatal("premise: the fresh limiter must admit its first holder, or this test sheds for the wrong reason")
	}
	// Never released: every later TryAcquire on this limiter must now fail.
	if p.obsLimiter.TryAcquire() {
		t.Fatal("premise: the limiter is not saturated, so the drop below would not be a drop")
	}

	w := dispatchCompress(t, p, "ws-shed", compressiblePrompt, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d — a shed observation must not affect the served response", w.Code)
	}
	if up.lastPrompt(t) == compressiblePrompt {
		t.Fatal("premise: the gate was open, so the rewritten prompt should still have gone upstream")
	}
	if rows := sink.all(); len(rows) != 0 {
		t.Errorf("recorded %d measurements with the writer bound saturated, want 0 — "+
			"if this ever passes with a row, the shed stopped being silent and the comment above is stale: %+v",
			len(rows), rows)
	}
	if p.obsLimiter.Dropped() == 0 {
		t.Error("the limiter recorded no drop — the row went missing for some other reason than the bound")
	}
}

// An unwired deployment must serve normally and write nothing — the capture is
// observational and may never reach the response.
func TestMeasure_NoSinkIsInertAndTheRequestStillServes(t *testing.T) {
	p, up, _ := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-nosink", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	w := dispatchCompress(t, p, "ws-nosink", compressiblePrompt, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d with no measurement sink wired", w.Code)
	}
	if up.lastPrompt(t) == compressiblePrompt {
		t.Errorf("premise: the gate was open, so the rewrite should still have been sent")
	}
}

// A sink that errors must not change what the caller got: the response is already
// flushed when the capture runs.
//
// ⚠ THE "EXACTLY ONCE" BELOW IS THE HALF THAT NEEDED A CONTROL, and it is the only
// assertion in this file that exercises the error branch at all. Positive control
// C19 of w61-measure-controls-4f2b.py makes that branch retry the write once: the
// count becomes 2 and only this test can see it, because every other sink here
// returns nil and never enters the branch.
func TestMeasure_ASinkErrorDoesNotAffectTheServedResponse(t *testing.T) {
	p, _, sink := newMeasuredProxy(t, workspace.Workspace{
		ID: "ws-err", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)
	sink.err = context.DeadlineExceeded

	w := dispatchCompress(t, p, "ws-err", compressiblePrompt, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d after a sink error", w.Code)
	}
	if len(sink.all()) != 1 {
		t.Errorf("the write was still attempted exactly once, got %d", len(sink.all()))
	}
}
