package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/compressor"
	"github.com/talyvor/lens/internal/fallback"
	"github.com/talyvor/lens/internal/guardrails"
	"github.com/talyvor/lens/internal/injection"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/pii"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
)

// ─── the capability gate on the STREAMING path ───
//
// THE CAPABILITY RULE HAS TWO CALL SITES IN THIS FILE'S PACKAGE AND #453
// GUARDED ONE OF THEM. The final-model gate (proxy.go, the routing path) is
// covered by modality_gate_nonimage_test.go; the STREAMING gate is a separate
// `if` — streaming skips the routing rewrite to preserve SSE wire-compatibility,
// so it enforces the rule itself and then returns. Every assertion that reached
// it was built from an `image_url` body: TestVisionStreamGate_StillRefusesA-
// GenuinelyTextOnlyModel is the only test on the gate at all, and its body is an
// image.
//
// MEASURED, NOT ASSUMED, WITH THE EXACT CI COMMAND (`go test -timeout 120s
// -race -count=1 -p 1 ./...`, real Postgres FROM ZERO): narrow the streaming
// gate from `modSet.Multimodal()` to `modSet.HasImage`, REMOVE THIS FILE, and
// the whole repo is GREEN — exit 0, 103 packages ok, 0 FAIL. That includes
// #453's census in internal/modality (it guards the BRANCHES of Supports, and
// every branch is still intact) and #453's own call-site cases (they go
// through the NON-streaming path). Only this file reds for it.
//
// ⚠ THE PACKAGE COUNT ABOVE SAID 98 AND THE REPO HAS 103 — corrected when this
// file's claims were re-measured from red rather than re-read. The count is the
// kind of figure that goes stale in the direction of looking fine.
//
// ⚠ AND THE FIRST RUN OF THAT REPO-WIDE CONTROL WAS A LIE THE OTHER WAY: run
// against a Postgres container that had already served SUBSET package runs, it
// came back with 140 reds across economy, stripe, bond, attribution and
// retention — and NOT ONE modality or proxy test among them. That is ci.yaml's
// own documented hazard (the gated integration tests DROP/CREATE their fixture
// tables), and it reads as CAUGHT when the guard under test did nothing. A
// repo-wide control here is only evidence if its database started empty.
//
// WHAT THE UNGUARDED NARROWING COSTS: an audio or document STREAM to a model
// that cannot serve it stops being refused, is forwarded upstream, and
// streamSpend writes a spend row — the caller is BILLED for content the model
// never received. That is the same consequence #453 measured one layer over.
//
// The product is CORRECT today, on every arm, measured on the wire before this
// file existed: audio→gpt-4o 422 spend=0, document→gpt-4o 422 spend=0,
// image→gpt-4 422 spend=0, and the served arms 200 spend=1. Nothing would have
// noticed if it stopped being true.
//
// SO THE RULES ARE DELIBERATELY NOT "TWO MORE STREAMING CASES", which would
// leave a fourth modality exactly as exposed as audio was:
//
//	rule S1 — a REFUSAL CENSUS over the table: for EVERY modality, a stream
//	          carrying it to a model that cannot serve it is 422, names the
//	          modality, and bills NOTHING.
//	rule S2 — a FLOOR: for EVERY modality, a stream carrying it to a model that
//	          CAN serve it is 200 and billed once. Without this, S1 is satisfied
//	          by a gate that refuses every multimodal stream — a gate that
//	          answers the same thing always. Measured: with `!Supports` dropped
//	          from the gate, all three S2 arms turn 422, and the non-streaming
//	          path stays 200, so these arms do traverse the gate under test and
//	          not some other one.
//	rule S3 — CLOSURE: the table covers exactly the `Has*` bool fields of
//	          modality.ModalitySet, read by reflection off the struct. Add
//	          `HasVideo` without a streaming fixture and S3 reds.
//	          ⚠ #453's census carries its OWN reflection over the same struct,
//	          so a new FIELD reds in both places and this rule is not what
//	          catches that. What only this rule can see, measured: a modality
//	          that IS gated on the non-streaming path and has no STREAMING
//	          fixture — drop audio from streamGatedModalities alone and this
//	          reds while the sibling census stays green. That case, not the
//	          new-field case, is why it is here.
//
// ⚠ EACH ARM'S CAPABLE MODEL IS ON A DIFFERENT PROVIDER because that is where
// the capability lives, not for coverage's sake: no OpenAI model in the catalog
// serves audio or documents, Anthropic serves documents, and google is the only
// audio-capable family. That is also why this file builds its own proxy —
// newLoggingProxy passes "" for the google key, and a keyless google request is
// refused 503 before it ever reaches the gate (measured).
//
// ⚠ EVERY BODY BELOW CARRIES A UNIQUE PROMPT. The exact-response cache is
// consulted BEFORE this gate, and reprice_override_vision_test.go records a
// measurement that was green for that reason alone. Each subtest also builds a
// fresh proxy, so the cache starts empty either way — belt and braces on a
// money path.
//
// The expected refusal strings are HARDCODED literals rather than read back
// from modality.Label() or the handler's own message, so a change to either has
// to be made here too.

// streamGatedModality names one modality field of modality.ModalitySet, a
// STREAMING body that carries it, and the two models that tell the gate's two
// answers apart. `field` is what rule S3 matches against the struct, so a
// rename cannot leave a stale entry pointing at nothing.
type streamGatedModality struct {
	name  string
	field string
	body  func(model, nonce string) string
	// The refusal arm (S1) and the served arm (S2). `handler` selects the
	// front door, because a model is only reachable through its provider's.
	incapableModel   string
	incapableHandler string
	capableModel     string
	capableHandler   string
	// capOf reads the capability this modality needs off a catalog entry, so
	// both arms can be proved non-vacuous against the shipped catalog rather
	// than trusted from the model id.
	capOf func(catalog.Capabilities) bool
}

func imageStreamBody(model, nonce string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is in this image? ` + nonce + `"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
}

func audioStreamBody(model, nonce string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is said in this clip? ` + nonce + `"},` +
		`{"type":"input_audio","input_audio":{"data":"SOMEBASE64AUDIO","format":"wav"}}]}]}`
}

func documentStreamBody(model, nonce string) string {
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"summarise this contract ` + nonce + `"},` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf"}}]}]}`
}

// gpt-4o is vision-capable and neither audio- nor document-capable, so it is
// the input that tells the three branches apart at the call site: an image
// stream at gpt-4o is SERVED and an audio or document stream at the same model
// is refused. The image arm needs a genuinely text-only model, and gpt-4 is
// absent from the catalog — the conservative-default case.
var streamGatedModalities = []streamGatedModality{
	{
		name: "image", field: "HasImage", body: imageStreamBody,
		incapableModel: "gpt-4", incapableHandler: "openai",
		capableModel: "gpt-4o", capableHandler: "openai",
		capOf: func(c catalog.Capabilities) bool { return c.Vision },
	},
	{
		name: "audio", field: "HasAudio", body: audioStreamBody,
		incapableModel: "gpt-4o", incapableHandler: "openai",
		capableModel: "gemini-2.5-flash", capableHandler: "google",
		capOf: func(c catalog.Capabilities) bool { return c.Audio },
	},
	{
		name: "document", field: "HasDocument", body: documentStreamBody,
		incapableModel: "gpt-4o", incapableHandler: "openai",
		capableModel: "claude-opus-5", capableHandler: "anthropic",
		capOf: func(c catalog.Capabilities) bool { return c.Document },
	},
}

// newStreamGateProxy is newLoggingProxy plus a google key and a google URL:
// the audio-capable population in the shipped catalog is entirely google, and
// without a key HandleGoogle answers 503 before the gate runs, which would make
// the audio floor measure the key check instead of the gate.
func newStreamGateProxy(t *testing.T) (*Proxy, *recordingAlertSink) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	t.Cleanup(srv.Close)

	exact, _ := newExactCacheForTest(t)
	wsm := workspace.New(nil)
	if err := wsm.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-log", Name: "stream-gate-test", Active: true, LoggingPolicy: workspace.LoggingMetadata,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	p := New(
		exact, nil, nil,
		compressor.New(), router.New(), pii.New(),
		nil, nil, nil, nil, wsm, nil, nil, nil, nil, nil, nil,
		fallback.New(), nil, nil, guardrails.New(pii.New(), injection.New(injection.DefaultPolicy())),
		"openai-key", "anthropic-key", "google-key",
	)
	p.openAIURL = srv.URL
	p.anthropicURL = srv.URL
	p.googleURL = srv.URL
	sink := &recordingAlertSink{}
	p.setAlertSink(sink)
	return p, sink
}

// dispatchModalityStream sends body through the named provider's front door and returns
// the recorder. An unknown handler is a hard failure rather than a silent
// fallback to OpenAI — a table entry pointing at a door that does not exist
// would otherwise measure the wrong path.
func dispatchModalityStream(t *testing.T, p *Proxy, handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	var path string
	var h func(http.ResponseWriter, *http.Request)
	switch handler {
	case "openai":
		path, h = "/v1/proxy/openai/v1/chat/completions", p.HandleOpenAI
	case "anthropic":
		path, h = "/v1/proxy/anthropic/v1/messages", p.HandleAnthropic
	case "google":
		path, h = "/v1/proxy/google/v1beta/models/gemini-2.5-flash:generateContent", p.HandleGoogle
	default:
		t.Fatalf("dispatchModalityStream: unknown handler %q — the table names a front door this helper cannot open", handler)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Talyvor-Workspace", "ws-log")
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// TestStreamGate_EveryModalityIsRefusedByAnIncapableModel is rule S1.
func TestStreamGate_EveryModalityIsRefusedByAnIncapableModel(t *testing.T) {
	for _, g := range streamGatedModalities {
		t.Run(g.name, func(t *testing.T) {
			// A refusal case against a model that can actually serve the
			// modality would be asserting the opposite of the product; a
			// refusal case against a model whose capability we never checked
			// is asserting nothing in particular.
			if g.capOf(catalog.CapabilitiesOf(g.incapableModel)) {
				t.Fatalf("%s is %s-capable in the shipped catalog — this refusal case is measuring nothing",
					g.incapableModel, g.name)
			}
			p, sink := newStreamGateProxy(t)
			w := dispatchModalityStream(t, p, g.incapableHandler, g.body(g.incapableModel, "s1-"+g.name))

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("streaming %s request to %s: status = %d, want 422; body=%s",
					g.name, g.incapableModel, w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "streaming request contains") {
				t.Fatalf("the refusal must come from the STREAMING gate, not some other check: %s", body)
			}
			if !strings.Contains(body, g.name) {
				t.Fatalf("the error must say WHICH modality was refused (want %q): %s", g.name, body)
			}
			if !strings.Contains(body, "does not support it") {
				t.Fatalf("the error must name the reason: %s", body)
			}
			// The whole point: a refused request is not a billed request.
			if sink.calls != 0 {
				t.Fatalf("a refused streaming %s request must record no spend: calls=%d", g.name, sink.calls)
			}
			if got := w.Header().Get("X-Talyvor-Modality"); got != g.name {
				t.Fatalf("detected modality header: got %q want %q", got, g.name)
			}
		})
	}
}

// TestStreamGate_EveryModalityIsServedByACapableModel is rule S2 — the floor.
// Must stay green: it is what stops S1 from being satisfied by a gate that
// refuses every multimodal stream, and it is the case a customer feels (traffic
// the model serves, refused).
func TestStreamGate_EveryModalityIsServedByACapableModel(t *testing.T) {
	for _, g := range streamGatedModalities {
		t.Run(g.name, func(t *testing.T) {
			if !g.capOf(catalog.CapabilitiesOf(g.capableModel)) {
				t.Fatalf("%s is NOT %s-capable in the shipped catalog — this floor would pass over a refusal, "+
					"leaving rule S1 satisfiable by a gate that always refuses", g.capableModel, g.name)
			}
			p, sink := newStreamGateProxy(t)
			w := dispatchModalityStream(t, p, g.capableHandler, g.body(g.capableModel, "s2-"+g.name))

			if w.Code != http.StatusOK {
				t.Fatalf("streaming %s request to the %s-capable %s: status = %d, want 200 — the gate is "+
					"refusing traffic the model serves; body=%s", g.name, g.name, g.capableModel, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "streaming request contains") {
				t.Fatalf("a capable model must not receive the capability refusal: %s", w.Body.String())
			}
			if sink.calls != 1 {
				t.Fatalf("a served streaming %s request must record exactly one spend row: calls=%d", g.name, sink.calls)
			}
		})
	}
}

// TestStreamGate_TableCoversEveryModalityField is rule S3: the table is checked
// against the struct, so a modality added to ModalitySet without a streaming
// fixture fails here rather than being silently ungated on this path. Read by
// reflection — a restated list would rot exactly like the citations this repo
// has already had to repair.
func TestStreamGate_TableCoversEveryModalityField(t *testing.T) {
	declared := map[string]bool{}
	rt := reflect.TypeOf(modality.ModalitySet{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Type.Kind() == reflect.Bool && strings.HasPrefix(f.Name, "Has") {
			declared[f.Name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Has* bool fields on ModalitySet — the reflection rule is reading nothing")
	}

	covered := map[string]bool{}
	for _, g := range streamGatedModalities {
		if !declared[g.field] {
			t.Errorf("streamGatedModalities names %q, which is not a Has* bool field of ModalitySet — "+
				"a renamed or removed modality has left a stale entry", g.field)
		}
		covered[g.field] = true
	}
	for f := range declared {
		if !covered[f] {
			t.Errorf("ModalitySet.%s is a modality with no entry in streamGatedModalities — the STREAMING "+
				"capability gate is unguarded for it, and a stream carrying it to an incapable model would "+
				"be forwarded and billed", f)
		}
	}
}

// TestStreamGate_DetectionSeesEachModalityInItsStreamingBody keeps the two
// tests above from being satisfiable by a body that carries no modality at all.
// If a fixture stopped parsing, Detect would return an all-false set, the gate
// would not run, and the refusal case would be the only thing that reds — with
// a message about a status code, pointing at the gate rather than at the
// fixture. This says which it is.
func TestStreamGate_DetectionSeesEachModalityInItsStreamingBody(t *testing.T) {
	for _, g := range streamGatedModalities {
		t.Run(g.name, func(t *testing.T) {
			set := modality.Detect([]byte(g.body(g.incapableModel, "detect-"+g.name)))
			if !set.Multimodal() {
				t.Fatalf("Detect saw no modality at all in the %s streaming fixture — the gate would never run "+
					"and both rules above would pass over a request carrying nothing", g.name)
			}
			if got := set.Label(); got != g.name {
				t.Fatalf("the %s streaming fixture detects as %q — a fixture carrying more (or other) modalities "+
					"than its entry claims cannot tell the gate's branches apart", g.name, got)
			}
		})
	}
}
