package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/workspace"
)

// A PRICE CHANGE MUST NOT REFUSE A REQUEST THE MODEL CAN SERVE.
//
// LENS_MODEL_CATALOG_OVERRIDES is documented in cmd/lens/main.go as the way to "reprice models
// without a rebuild", and it is the remedy WarnUnpricedModel prints on every unpriced request. An
// operator following it states a price. Before catalog.DecodeOverrides, that document was decoded
// into a FRESH catalog.Model, so every field the operator did not mention arrived as its zero value
// and replaced the seeded truth — including Capabilities.
//
// The proxy enforces the capability gate on the streaming path (proxy.go: streaming skips the routing
// rewrite, so a multimodal stream to a model that cannot serve it must fail fast). A vision model
// whose capabilities were blanked by a reprice therefore starts returning 422 for exactly the traffic
// it served the minute before.
//
// This is measured through HandleOpenAI — the real entry point — because the interesting number is
// the STATUS CODE the customer receives, not the boolean modality.Supports returns.
//
// ⚠ EVERY REQUEST BELOW CARRIES A UNIQUE PROMPT. An identical body is served from the exact-response
// cache BEFORE this gate runs, and the first version of this measurement was green for that reason
// alone — a cache hit standing in for a capability the catalog had already lost.
func TestRepriceOverride_DoesNotWithdrawVisionFromTheServingPath(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	visionStream := func(nonce string) *httptest.ResponseRecorder {
		body := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"what is in this image? ` + nonce + `"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", "ws-log")
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w
	}

	seeded, ok := catalog.Get("gpt-4o")
	if !ok {
		t.Fatal("the shipped catalog has no gpt-4o — this test is measuring nothing")
	}
	if !seeded.Capabilities.Vision {
		t.Fatalf("gpt-4o is seeded without vision (%+v) — this test is measuring nothing", seeded.Capabilities)
	}
	// The global registry is process-wide; put the seeded entry back whatever happens below.
	t.Cleanup(func() { catalog.Override(seeded) })

	// CONTROL: the request is served before any override. Without this, a 422 afterwards could just
	// mean the harness never could serve a vision stream.
	if w := visionStream("control"); w.Code != http.StatusOK {
		t.Fatalf("CONTROL (no override): status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The documented operator action, decoded exactly as cmd/lens decodes it.
	overrides, err := catalog.DecodeOverrides([]byte(`[{"id":"gpt-4o","input_per_1m":3.75,"output_per_1m":15.00}]`))
	if err != nil {
		t.Fatalf("DecodeOverrides: %v", err)
	}
	catalog.LoadOverrides(overrides)

	if w := visionStream("after-reprice"); w.Code != http.StatusOK {
		t.Errorf("after a PRICE-ONLY override the same streaming vision request returns %d (body=%s) — "+
			"repricing gpt-4o withdrew its vision capability and the customer is refused traffic the "+
			"model serves", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// The reprice must actually have taken effect: a guard that passes because the override did
	// nothing at all is not a guard.
	if in, _, _, out, ok := catalog.PriceDetailed("gpt-4o"); !ok || in != 3.75 || out != 15.00 {
		t.Errorf("PriceDetailed(gpt-4o) = %v/%v ok=%v after the reprice, want 3.75/15.00 — the override "+
			"did not apply, so the 200 above proves nothing", in, out, ok)
	}
}

// THE GATE ITSELF MUST STILL BITE. Must-stay-green companion to the case above: a model that genuinely
// cannot serve images is still refused, so "200 after a reprice" cannot be satisfied by a gate that
// stopped working.
func TestVisionStreamGate_StillRefusesAGenuinelyTextOnlyModel(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)

	if caps := catalog.CapabilitiesOf("gpt-4"); caps.Vision {
		t.Fatalf("gpt-4 is seeded WITH vision (%+v) — pick a different text-only model for this control", caps)
	}
	body := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"text-only model, image attached"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("streaming image request to a text-only model: status = %d, want 422 — the capability "+
			"gate has stopped biting, which would make the reprice case above vacuous", w.Code)
	}
}
