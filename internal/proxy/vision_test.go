package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/workspace"
)

func imageBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is in this image?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,SOMEBASE64DATA"}}]}]}`
}

func dispatchBody(t *testing.T, p *Proxy, wsID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", wsID)
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	return w
}

// A pinned, text-only model receiving an image must FAIL FAST with a clear
// error — never silently strip the image and answer from text.
func TestVision_PinnedTextOnlyModelFailsFast(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	// "gpt-4" is not in the capability registry → conservative text-only.
	w := dispatchBody(t, p, "ws-log", imageBody("gpt-4"))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "does not support") {
		t.Fatalf("error should clearly explain the unsupported modality: %s", w.Body.String())
	}
	if sink.calls != 0 {
		t.Fatalf("a failed-fast request must not record spend: calls=%d", sink.calls)
	}
}

// A capable model serves the image and the spend row is tagged with the
// modality. This fake upstream surfaces no usage, so the row falls back to
// the modality-aware estimate (marked estimated); with real provider usage
// the same path bills exact — see
// TestSpend_NonStreamingMultimodalUsesProviderUsageNotFlat1000.
func TestVision_CapableModelPassesAndRecordsModality(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	w := dispatchBody(t, p, "ws-log", imageBody("gpt-4o")) // gpt-4o family = vision

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Talyvor-Modality") != "image" {
		t.Fatalf("response should advertise the detected modality: %q", w.Header().Get("X-Talyvor-Modality"))
	}
	if sink.lastModality != "image" {
		t.Fatalf("spend record modality: got %q want image", sink.lastModality)
	}
	if !sink.lastEstimated {
		t.Fatal("multimodal spend without provider usage must fall back to the estimate")
	}
}

// An auto-route image request is served by a model that can see the image
// (not failed, not silently text-only). B17.116: "auto" now resolves to the
// router's pick before the gate, so a capable pick needs no redirect.
func TestVision_AutoRouteRedirectsToCapable(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	w := dispatchBody(t, p, "ws-log", imageBody("auto"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if served := servedModel(t, sink); !modality.Supports(served, modality.ModalitySet{HasImage: true}) {
		t.Fatalf("the auto image request was served by %q, a model that cannot see images", served)
	}
	if sink.lastModality != "image" {
		t.Fatalf("spend modality after redirect: got %q want image", sink.lastModality)
	}
}

// A text-only request is unaffected — no modality gating, normal path.
func TestVision_TextRequestUnaffected(t *testing.T) {
	p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	w := dispatchBody(t, p, "ws-log", `{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("text request status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if sink.lastModality != "text" {
		t.Fatalf("text request modality: got %q want text", sink.lastModality)
	}
	// This fake upstream returns no usage block, so the row is HONESTLY
	// marked estimated (len/4 fallback). In production the provider returns
	// a usage object and the row is billed exact / not-estimated — see
	// TestSpend_NonStreamingOpenAIUsageBilledExact.
	if !sink.lastEstimated {
		t.Fatal("text request without provider usage must fall back to an estimate (marked estimated)")
	}
}
