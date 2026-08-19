package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// ─── the capability gate on the wire, for the modalities that are not images ───
//
// The gate at the FINAL-model check is a money path: refusing costs nothing,
// and letting an incapable model through writes a spend row for content the
// model never received. Every proxy assertion on that gate was built from an
// `image_url` body (vision_test.go, reprice_override_vision_test.go,
// modality_redirect_reach_test.go), so the audio and document arms were
// enforced by nothing here.
//
// WHY THESE CASES ARE NOT A DUPLICATE OF internal/modality's census. That
// census guards the BRANCHES of Supports. It cannot see the CALL SITE: narrow
// this file's gate from `modSet.Multimodal()` to `modSet.HasImage` and the
// census stays green, every existing image test stays green, and an audio
// request is served 200 and billed. These cases are the only thing that reds
// for that — measured, not assumed (control M4).
//
// The expected refusals below are HARDCODED literals rather than read back
// from modality.Label() or the handler's own message, so a change to either
// has to be made here too.

func audioBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is said in this clip?"},` +
		`{"type":"input_audio","input_audio":{"data":"SOMEBASE64AUDIO","format":"wav"}}]}]}`
}

func documentBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"summarise this contract"},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}]}]}`
}

// A pinned model that cannot serve the modality must fail fast and bill
// NOTHING — for audio and document exactly as for images. "gpt-4o" is
// vision-capable and neither audio- nor document-capable, so it is the input
// that tells the three branches apart: an image at gpt-4o is served, and
// these are refused.
func TestModalityGate_PinnedIncapableModelFailsFastAndBillsNothing(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		modality string
	}{
		{"audio", audioBody("gpt-4o"), "audio"},
		{"document", documentBody("gpt-4o"), "document"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
			w := dispatchBody(t, p, "ws-log", c.body)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "does not support") {
				t.Fatalf("the error must name the unsupported modality: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.modality) {
				t.Fatalf("the error must say WHICH modality was refused (want %q): %s", c.modality, w.Body.String())
			}
			// The whole point: a refused request is not a billed request.
			if sink.calls != 0 {
				t.Fatalf("a failed-fast %s request must record no spend: calls=%d", c.name, sink.calls)
			}
			if got := w.Header().Get("X-Talyvor-Modality"); got != c.modality {
				t.Fatalf("detected modality header: got %q want %q", got, c.modality)
			}
		})
	}
}

// Auto-route is the arm that redirects rather than refuses — but only when
// the provider HAS a capable model. No OpenAI model in the catalog serves
// audio or documents, so auto-route must still fail fast here and must not
// redirect to a model that cannot serve the content. This is the case that
// distinguishes "redirected somewhere capable" from "redirected anywhere".
func TestModalityGate_AutoRouteWithNoCapableModelRefusesRatherThanRedirects(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     string
		modality string
	}{
		{"audio", audioBody("auto"), "audio"},
		{"document", documentBody("auto"), "document"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, sink, _ := newLoggingProxy(t, workspace.LoggingMetadata)
			w := dispatchBody(t, p, "ws-log", c.body)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "no configured openai model supports it") {
				t.Fatalf("auto-route with nothing capable must say so: %s", w.Body.String())
			}
			if redirect := w.Header().Get("X-Talyvor-Vision-Redirect"); redirect != "" {
				t.Fatalf("must not redirect a %s request to a model that cannot serve it: %q", c.name, redirect)
			}
			if sink.calls != 0 {
				t.Fatalf("a refused auto-route %s request must record no spend: calls=%d", c.name, sink.calls)
			}
		})
	}
}
