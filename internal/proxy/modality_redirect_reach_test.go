package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/modality"
	"github.com/talyvor/lens/internal/workspace"
)

// THE REDIRECT'S REACHABLE SET IS SMALLER THAN THE CATALOG'S CAPABLE SET, AND THE REFUSAL IT SENDS
// SAYS OTHERWISE.
//
// proxy.go's auto-route branch calls modality.CapableModel and, when it comes back ok=false, answers
// 422 "request contains image content but no configured openai model supports it". CapableModel walks
// modality.providerPreference — a HARDCODED six-entry list for openai — and intersects it with the
// workspace's AllowedModels. The catalog records SIXTEEN vision-capable openai models. So a workspace
// whose allow-list holds a capable model that is not one of the curated six is refused, and told that
// nothing is configured that can serve it, while the model it is allowed to use can.
//
// ⚠ MEASURED THROUGH HandleOpenAI, because the interesting thing is the STATUS CODE AND THE SENTENCE
// THE CUSTOMER RECEIVES, not the boolean CapableModel returns.
//
// ⚠ THIS PINS THE HOLE; IT DOES NOT CLOSE IT, AND THAT IS DELIBERATE. providerPreference documents
// itself as routing POLICY — "the models we'd prefer to redirect a multimodal request to,
// cheapest-capable first ... intentionally omits the nano tier as a redirect target even though it's
// vision-capable". Widening the redirect to any capable model of the provider would override a stated
// policy decision (and would start sending traffic to the tier that omission excluded), and choosing
// WHICH capable model a customer's image lands on is a routing-and-cost call, not a session's. THE
// DECISION, stated so it can be taken rather than rediscovered: (a) widen the redirect to the
// workspace's allowed capable models when the curated order yields none — the smallest change, turns
// a 422 into a serve and never alters an existing redirect; (b) leave the policy and make the message
// name the real reason ("no openai model in the redirect preference list is allowed for this
// workspace"), which is honest and still refuses; or (c) leave both and accept that an allow-list
// outside the curated six disables image traffic on auto-route.
//
// ⚠ THE PIN EXPIRES ON ITS OWN FIX: if the 422 ever becomes a 200 this test FAILS and names the
// decision, so the change cannot land silently as a behaviour drift.
//
// ⚠ AND THE COMMENT THAT SENT THIS THE WRONG WAY IS CORRECTED IN THE SAME MERGE: catalog.ByProvider
// documented itself as "the order the modality redirect uses to pick a capable model" and has ZERO
// production callers.
func TestModalityRedirect_ReachableSetIsNarrowerThanTheCatalog(t *testing.T) {
	need := modality.ModalitySet{HasImage: true}
	const pinned = "gpt-5.5" // vision-capable, provider openai, NOT in providerPreference

	// ── CONTROL A: the catalog really does record this model as capable, from this provider. ────
	m, ok := catalog.Get(pinned)
	if !ok || !m.Capabilities.Vision || m.Provider != "openai" {
		t.Fatalf("CONTROL A: catalog %s = %+v ok=%v — pick another capable non-preferred model; "+
			"this test is measuring nothing", pinned, m, ok)
	}
	if !modality.Supports(pinned, need) {
		t.Fatalf("CONTROL A: modality.Supports(%s, image) is false — the capability gate itself would "+
			"refuse this model and the refusal below would be correct", pinned)
	}

	// ── CONTROL B: the curated order CAN answer, so ok=false below is about reachability, not about
	// a provider that has no capable model at all (which is the honest case for groq and mistral,
	// neither of which has a single multimodal model in this catalog).
	if got, okAny := modality.CapableModel("openai", need, nil); !okAny {
		t.Fatalf("CONTROL B: CapableModel(openai, image, nil) = %q ok=false — the preference list is "+
			"empty or broken, and every assertion below would pass for the wrong reason", got)
	}

	// ── THE MEASUREMENT, unit level: the only allowed model is capable, and it is unreachable. ──
	if got, okAllowed := modality.CapableModel("openai", need, []string{pinned}); okAllowed {
		t.Errorf("CapableModel(openai, image, [%s]) = %q ok=true — the redirect can now reach a model "+
			"outside providerPreference. That is decision (a) in this file's header; take it "+
			"deliberately and delete this pin, do not let it land as a drift.", pinned, got)
	}

	// ── AND THROUGH THE REAL ENTRY POINT, which is where the customer meets it. ─────────────────
	p, _, _ := newLoggingProxy(t, workspace.LoggingMetadata)
	// ⚠ "auto" is in AllowedModels DELIBERATELY. workspace.CheckPolicy tests the REQUESTED model
	// against the allow-list before any routing runs, and the requested model on an auto-route
	// request is the literal string "auto" — so a workspace with an allow-list that does not name it
	// is refused 403 "model \"auto\" not allowed" and never reaches the capability gate at all.
	// Without this line the case below measures the 403 and not the 422.
	if err := p.workspaceManager.RegisterWorkspace(context.Background(), workspace.Workspace{
		ID: "ws-pinned-reach", Name: "pinned-reach", Active: true,
		LoggingPolicy: workspace.LoggingMetadata,
		AllowedModels: []string{"auto", pinned},
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}

	// ⚠ EVERY REQUEST CARRIES A UNIQUE PROMPT: an identical body is served from the exact-response
	// cache before this gate runs, and a cache hit would stand in for a capability decision.
	image := func(wsID, nonce string) *httptest.ResponseRecorder {
		body := `{"model":"auto","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"what is in this image? ` + nonce + `"},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Talyvor-Workspace", wsID)
		w := httptest.NewRecorder()
		p.HandleOpenAI(w, req)
		return w
	}

	// ── CONTROL C: with no allow-list the same request is redirected and SERVED. Without this, the
	// 422 below could just mean the harness cannot serve an image request at all.
	unpinned := image("ws-log", "no-allow-list")
	if unpinned.Code != http.StatusOK {
		t.Fatalf("CONTROL C: an auto-route image request on a workspace with NO allow-list returned "+
			"%d (%s) — the harness cannot serve this shape and nothing below means anything",
			unpinned.Code, strings.TrimSpace(unpinned.Body.String()))
	}
	if redirect := unpinned.Header().Get("X-Talyvor-Vision-Redirect"); redirect == "" {
		t.Fatalf("CONTROL C: served 200 with no X-Talyvor-Vision-Redirect header — the redirect did " +
			"not run, so this is not the path under test")
	}

	got := image("ws-pinned-reach", "allow-list-of-one-capable-model")
	if got.Code == http.StatusOK {
		t.Errorf("PIN EXPIRED: an auto-route image request from a workspace allowed only %s is now "+
			"SERVED (redirect=%q). If that was decided, delete this test and record the decision; if "+
			"it was not, the redirect started sending traffic to a model providerPreference excludes.",
			pinned, got.Header().Get("X-Talyvor-Vision-Redirect"))
		return
	}
	if got.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 or 200; body=%s", got.Code, strings.TrimSpace(got.Body.String()))
	}
	// The sentence itself is the finding: it is a claim about what is CONFIGURED, and the workspace
	// has configured exactly one model, which is capable.
	if body := got.Body.String(); !strings.Contains(body, "no configured openai model supports it") {
		t.Errorf("422 body = %s — the refusal message changed. If it now names the real reason "+
			"(the preference list, not the configuration) that is decision (b) and this pin should be "+
			"retargeted rather than deleted.", strings.TrimSpace(body))
	}
	t.Logf("PINNED: workspace allowed=[auto %s] (vision-capable, provider openai) -> %d %s",
		pinned, got.Code, strings.TrimSpace(got.Body.String()))
}

// THE POPULATION, MEASURED RATHER THAN COUNTED BY HAND.
//
// Stated as a structural property, not as the two integers it produces today: an exact count reds on
// every seed edit and says nothing, while "there exists a capable model this redirect cannot reach"
// is the fact that matters and goes green exactly when the hole closes.
func TestModalityRedirect_EveryProviderWithCapableModelsIsInThePreferenceMap(t *testing.T) {
	need := modality.ModalitySet{HasImage: true}

	capableByProvider := map[string][]string{}
	for _, m := range catalog.All() {
		if m.Capabilities.Vision {
			capableByProvider[m.Provider] = append(capableByProvider[m.Provider], m.ID)
		}
	}
	if len(capableByProvider) == 0 {
		t.Fatal("CONTROL: no vision-capable model in the whole catalog — this test is measuring nothing")
	}

	unreachableSomewhere := false
	for provider, ids := range capableByProvider {
		// (1) A provider with capable models must be answerable at all. This is the half that is TRUE
		// today and would be a hard defect if it broke: a provider missing from the map refuses every
		// multimodal request with "no configured <provider> model supports it".
		if _, ok := modality.CapableModel(provider, need, nil); !ok {
			t.Errorf("provider %q has %d vision-capable models in the catalog (%v) and "+
				"CapableModel cannot answer for it at all — every multimodal auto-route request to "+
				"this provider is refused 422", provider, len(ids), ids)
		}
		// (2) The reachable set. Each capable model is offered as the workspace's ONLY allowed model;
		// if the redirect cannot reach it, a workspace allowed exactly that model is refused.
		var unreachable []string
		for _, id := range ids {
			if _, ok := modality.CapableModel(provider, need, []string{id}); !ok {
				unreachable = append(unreachable, id)
			}
		}
		if len(unreachable) > 0 {
			unreachableSomewhere = true
		}
		t.Logf("provider=%-10q capable=%d unreachable-as-sole-allowed=%d %v",
			provider, len(ids), len(unreachable), unreachable)
	}

	if !unreachableSomewhere {
		t.Errorf("PIN EXPIRED: every vision-capable model in the catalog is now reachable by the " +
			"redirect. That is decision (a) in modality_redirect_reach_test.go's header — record it " +
			"and delete both pins; do not leave a test asserting a hole that has been closed.")
	}
	// ⚠ groq and mistral are absent from this loop entirely and that is CORRECT, not an oversight:
	// measured over the seeded catalog, neither has a single model with any non-text capability, so
	// providerPreference's four keys are exactly the four providers that need one. Rule (1) is what
	// keeps that true the day one is added.
}
