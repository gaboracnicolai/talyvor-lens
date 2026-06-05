package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// newDistillSpendProxy builds a proxy with a wired distiller + a recording alert
// sink, so tests can assert the DISTILL attribution written to token_events. The
// upstream always replies with upstreamBody (it serves BOTH the vision-OCR
// sub-call and the main request).
func newDistillSpendProxy(t *testing.T, conv distill.IsolatedConverter, upstreamBody string, policy workspace.DistillPolicy) (*Proxy, *recordingAlertSink) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "distill-spend", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, DistillPolicy: policy,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "",
	)
	p.openAIURL = srv.URL
	p.anthropicURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	p.SetDistiller(conv, nil)
	return p, sink
}

func dispatchAnthropicDoc(t *testing.T, p *Proxy, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	w := httptest.NewRecorder()
	p.HandleAnthropic(w, req)
	return w
}

// A distilled TEXT-conversion request: the (lower-count) spend row is tagged
// distill_method='convert'. The saving stays IMPLICIT in the lower count — there
// is no separate saving write, and no vision_ocr row.
func TestSpend_DistilledRequestTaggedConvert(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "# clean markdown"}}
	p, sink := newDistillSpendProxy(t, conv,
		`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":50,"output_tokens":10}}`,
		workspace.DistillAlways)

	w := dispatchAnthropicDoc(t, p, anthropicDocBody(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sink.spendWithMethod("convert"); !ok {
		t.Errorf("a distilled request must record a spend row tagged 'convert'; rows=%+v", sink.spends)
	}
	if _, ok := sink.spendWithMethod("vision_ocr"); ok {
		t.Error("a text-conversion request must NOT record a vision_ocr row")
	}
}

// A non-distilled request (no document) records an UNTAGGED row (distill_method
// ”) — existing behavior is unaffected.
func TestSpend_NonDistilledUntagged(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "unused"}}
	p, sink := newDistillSpendProxy(t, conv,
		`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":1}}`,
		workspace.DistillAlways)

	// No document block → nothing to distill.
	body := []byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello"}]}`)
	w := dispatchAnthropicDoc(t, p, body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(sink.spends) != 1 || sink.spends[0].distillMethod != "" {
		t.Errorf("non-distilled traffic must record exactly one untagged row; rows=%+v", sink.spends)
	}
}

// A vision-OCR request: the OCR sub-call's cost is its OWN spend row tagged
// distill_method='vision_ocr', priced on the vision model, flagged estimated,
// and NEVER blended into the 'convert' main row.
func TestSpend_VisionOCRRecordsSeparateRow(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{NeedsVision: true}}
	// This anthropic body serves the vision OCR sub-call (text + usage) AND the
	// main request that proceeds with the recovered text.
	p, sink := newDistillSpendProxy(t, conv,
		`{"content":[{"type":"text","text":"# OCR recovered"}],"usage":{"input_tokens":1000,"output_tokens":40}}`,
		workspace.DistillAlways)

	w := dispatchAnthropicDoc(t, p, anthropicDocBody(t, false))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The main row still tags 'convert' (the model saw the OCR'd text).
	if _, ok := sink.spendWithMethod("convert"); !ok {
		t.Errorf("a vision request still records a 'convert' main row; rows=%+v", sink.spends)
	}
	// The OCR cost is its OWN row.
	vrow, ok := sink.spendWithMethod("vision_ocr")
	if !ok {
		t.Fatalf("the OCR cost must be recorded as its own vision_ocr row; rows=%+v", sink.spends)
	}
	if vrow.inputTokens != 1000 || vrow.outputTokens != 40 {
		t.Errorf("vision_ocr row must carry the REAL OCR token cost; got %d/%d want 1000/40", vrow.inputTokens, vrow.outputTokens)
	}
	if !vrow.estimated {
		t.Error("vision_ocr rows must be flagged cost_estimated")
	}
	if vrow.model != "claude-haiku-4-6" {
		t.Errorf("vision_ocr row must be priced on the vision model; got %q", vrow.model)
	}
	if vrow.modality != "document" {
		t.Errorf("vision_ocr row modality should be 'document' (the OCR input); got %q", vrow.modality)
	}
}
