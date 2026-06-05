package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/workspace"
)

var errInjected = errors.New("worker exploded")

type fakeDistillConv struct {
	calls int
	res   distill.Result
	err   error
}

func (f *fakeDistillConv) Convert(_ context.Context, _ []byte, format distill.Format) (distill.Result, error) {
	f.calls++
	r := f.res
	r.Format = format
	return r, f.err
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// anthropic document request: a user text block + a base64 PDF document block.
func anthropicDocBody(t *testing.T, stream bool) []byte {
	t.Helper()
	m := map[string]any{
		"model":  "claude-x",
		"stream": stream,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "summarize this"},
				map[string]any{"type": "document", "source": map[string]any{
					"type": "base64", "media_type": "application/pdf", "data": b64("%PDF-fake-bytes"),
				}},
			}},
		},
	}
	b, _ := json.Marshal(m)
	return b
}

func newDistiller(t *testing.T, conv distill.IsolatedConverter, policy workspace.DistillPolicy) *distillIntegration {
	t.Helper()
	wm := workspace.New(nil)
	if err := wm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws1", Name: "WS", Active: true, DistillPolicy: policy,
	}); err != nil {
		t.Fatal(err)
	}
	return &distillIntegration{converter: conv, cache: nil, wsManager: wm}
}

func reqWith(distillHeader string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/chat/completions", nil)
	if distillHeader != "" {
		r.Header.Set("X-Talyvor-Distill", distillHeader)
	}
	return r
}

// opted-in + document → the document is converted (isolated), the base64 blob is
// gone, the Markdown reaches the prompt, and modality becomes text-only.
func TestMaybeDistill_AnthropicDoc_OptIn(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "# Doc\n\nthe content"}}
	d := newDistiller(t, conv, workspace.DistillOptIn)
	body := anthropicDocBody(t, false)

	newBody, newPrompt, newMod, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if !did {
		t.Fatal("opted-in document request must be distilled")
	}
	if conv.calls != 1 {
		t.Fatalf("isolated converter should run once; calls=%d", conv.calls)
	}
	if bytes.Contains(newBody, []byte(b64("%PDF-fake-bytes"))) {
		t.Error("the base64 document blob must be GONE from the rewritten body")
	}
	if !strings.Contains(newPrompt, "the content") || !strings.Contains(newPrompt, "summarize this") {
		t.Errorf("prompt must carry the user text + the markdown; got %q", newPrompt)
	}
	if newMod.HasDocument {
		t.Error("after distillation the request must be text-only (HasDocument=false)")
	}
	if !json.Valid(newBody) {
		t.Error("rewritten body must be valid JSON")
	}
}

// NOT opted-in (policy opt_in, no header) → byte-for-byte inert.
func TestMaybeDistill_NotOptedIn_Inert(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "should not run"}}
	d := newDistiller(t, conv, workspace.DistillOptIn)
	body := anthropicDocBody(t, false)

	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith(""), body, "ws1", modality.Detect(body), nil)
	if did {
		t.Fatal("not-opted-in request must NOT be distilled")
	}
	if conv.calls != 0 {
		t.Errorf("converter must not run for a non-opted-in request; calls=%d", conv.calls)
	}
	if !bytes.Equal(newBody, body) {
		t.Error("inert: body must be byte-for-byte unchanged")
	}
}

// Policy disabled → inert even with the header.
func TestMaybeDistill_PolicyDisabled_Inert(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "x"}}
	d := newDistiller(t, conv, workspace.DistillDisabled)
	body := anthropicDocBody(t, false)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if did || conv.calls != 0 || !bytes.Equal(newBody, body) {
		t.Errorf("disabled workspace must be fully inert; did=%v calls=%d bodyChanged=%v", did, conv.calls, !bytes.Equal(newBody, body))
	}
}

// No document present → inert (nothing to distill), even when always-on.
func TestMaybeDistill_NoDocument_Inert(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "x"}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"just text"}]}`)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if did || conv.calls != 0 || !bytes.Equal(newBody, body) {
		t.Errorf("a request with no document must be inert; did=%v calls=%d", did, conv.calls)
	}
}

// DistillAlways → distilled without the header.
func TestMaybeDistill_Always_NoHeader(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "# md"}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	body := anthropicDocBody(t, false)
	_, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith(""), body, "ws1", modality.Detect(body), nil)
	if !did {
		t.Error("DistillAlways must distill a document with no header")
	}
}

// Conversion failure → graceful: original request passes through untouched.
func TestMaybeDistill_ConvError_Inert(t *testing.T) {
	conv := &fakeDistillConv{err: errInjected}
	d := newDistiller(t, conv, workspace.DistillAlways)
	body := anthropicDocBody(t, false)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if did {
		t.Fatal("a conversion failure must NOT rewrite the request")
	}
	if !bytes.Equal(newBody, body) {
		t.Error("a conversion failure must leave the body byte-for-byte unchanged")
	}
}

// With NO live dispatcher (nil), a NeedsVision document is left in place; since
// it's the only doc, the request is inert (the pre-stage-3 behavior).
func TestMaybeDistill_NeedsVision_Inert(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{NeedsVision: true, Markdown: ""}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	body := anthropicDocBody(t, false)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if did {
		t.Fatal("a NeedsVision document must pass through when no dispatcher is wired")
	}
	if !bytes.Equal(newBody, body) {
		t.Error("NeedsVision: body must be unchanged")
	}
}

// mockVision is a stub distill.VisionDispatcher for the wiring tests.
type mockVision struct {
	res distill.VisionResult
	err error
}

func (m mockVision) DispatchVision(context.Context, distill.VisionRequest) (distill.VisionResult, error) {
	return m.res, m.err
}

// A NeedsVision document + a LIVE dispatcher → the document block is OCR'd and
// the recovered Markdown reaches the prompt (the stage-5 seam, live).
func TestMaybeDistill_NeedsVision_LiveVisionOCR(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{NeedsVision: true}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	vis := mockVision{res: distill.VisionResult{Markdown: "# OCR\n\nrecovered text", InputTokens: 900, OutputTokens: 30, Model: "claude-haiku-4-6"}}
	body := anthropicDocBody(t, false)
	_, newPrompt, _, did, vs := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), vis)
	if !did {
		t.Fatal("a NeedsVision document with a live dispatcher must be OCR'd and rewritten")
	}
	if !strings.Contains(newPrompt, "recovered text") {
		t.Errorf("OCR'd Markdown must reach the prompt; got %q", newPrompt)
	}
	// The OCR sub-call cost must be SURFACED for a durable 'vision_ocr' spend row.
	if !vs.recorded() || vs.inputTokens != 900 || vs.outputTokens != 30 || vs.model != "claude-haiku-4-6" {
		t.Errorf("vision spend must be surfaced (model + real token split); got %+v", vs)
	}
}

// A NeedsVision document + a FAILING dispatcher → graceful: the original document
// block passes through byte-for-byte (a vision outage never fails a request).
func TestMaybeDistill_NeedsVision_VisionFailureGraceful(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{NeedsVision: true}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	vis := mockVision{err: errors.New("no capable model")}
	body := anthropicDocBody(t, false)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), vis)
	if did {
		t.Fatal("a vision failure must NOT rewrite the request")
	}
	if !bytes.Equal(newBody, body) {
		t.Error("vision failure: body must be byte-for-byte unchanged")
	}
}

// OpenAI data-URI document → distilled.
func TestMaybeDistill_OpenAIDataURI(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "# from data uri"}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	m := map[string]any{
		"model": "gpt-x",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:application/pdf;base64," + b64("%PDF-x"),
			}},
		}}},
	}
	body, _ := json.Marshal(m)
	_, newPrompt, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if !did {
		t.Fatal("OpenAI data-URI document must be distilled")
	}
	if !strings.Contains(newPrompt, "from data uri") {
		t.Errorf("markdown must reach prompt; got %q", newPrompt)
	}
}

// An IMAGE block (image/png) is NOT a document — it passes through untouched
// (it belongs to the vision path, not distillation).
func TestMaybeDistill_ImageNotDistilled(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "should not be used"}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	m := map[string]any{
		"model": "m",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": b64("PNGDATA"),
			}},
		}}},
	}
	body, _ := json.Marshal(m)
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if did || conv.calls != 0 {
		t.Errorf("an image (not a document) must not be distilled; did=%v calls=%d", did, conv.calls)
	}
	if !bytes.Equal(newBody, body) {
		t.Error("image request body must be unchanged")
	}
}

// A misconfigured distiller (nil deps) must be inert, never panic — the
// graceful/inert contract on the shared request path.
func TestMaybeDistill_NilDeps_Inert(t *testing.T) {
	body := anthropicDocBody(t, false)
	for _, d := range []*distillIntegration{
		{converter: nil, wsManager: nil},
		{converter: &fakeDistillConv{}, wsManager: nil},
	} {
		nb, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
		if did || !bytes.Equal(nb, body) {
			t.Errorf("nil-deps distiller must be inert; did=%v changed=%v", did, !bytes.Equal(nb, body))
		}
	}
}

// The stream flag must survive the rewrite (the streaming decision was locked
// before distillation).
func TestMaybeDistill_PreservesStreamFlag(t *testing.T) {
	conv := &fakeDistillConv{res: distill.Result{Markdown: "# md"}}
	d := newDistiller(t, conv, workspace.DistillAlways)
	body := anthropicDocBody(t, true) // stream:true
	newBody, _, _, did, _ := d.MaybeDistill(context.Background(), reqWith("true"), body, "ws1", modality.Detect(body), nil)
	if !did {
		t.Fatal("should distill")
	}
	var m map[string]any
	_ = json.Unmarshal(newBody, &m)
	if s, _ := m["stream"].(bool); !s {
		t.Error("the rewritten body must preserve stream:true")
	}
}
