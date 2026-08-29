package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/config"
	"strings"
	"testing"
)

// THE UNBILLED BATCH LANE.
//
// /v1/batch/submit calls Anthropic directly and never enters serve(). Talyvor pays the provider;
// the workspace is charged nothing. The package documented its own gap — "a nil hook (the default)
// means today's behaviour — a completed job that debits nothing" — and BatchRouter.SetSettleHook
// has NO production caller, so the hook is nil in every running Lens.
//
// ⚠ THE BILLING GAP IS NOT THE WORST OF IT, and that is what decided the fix. Reading the route:
//
//   · the workspace comes from a CLIENT HEADER (X-Talyvor-Workspace), defaulting to "default",
//     and is never checked against the caller's own workspace — so even a wired settle would bill
//     a workspace the caller named;
//   · /v1/batch/jobs returns batchRouter.ListJobs() with NO workspace filter at all, and BatchJob
//     carries `Prompt string` and `Response []byte` — so any authenticated caller can read every
//     workspace's batch prompts and answers.
//
// Wiring a settle onto that would put money on top of a cross-tenant read. The lane is switched
// OFF at the route, the way the economy master switch does it: unregistered, so chi serves its
// native 404 and nothing distinguishes it from a path that never existed.
//
// ⚠ THESE ASSERT THE INVARIANT, NOT A STATUS CODE. The rule is "a batch submission produces a
// spend row, or it does not happen". A test that accepts a 4xx would pass on a route that returned
// an error AFTER calling Anthropic.

// TestBatchLane_NoBareRouteRegistration: every /v1/batch route must go through the gate. A bare
// authed.Post is exactly how this shipped unbilled, so the shape itself is what is banned.
//
// ⚠ IT WAS A REGEX KNOWING THREE VERBS ON ONE LINE UNTIL #522. Measured: a bare route written
// with authed.Put, with r.Handle (the UNAUTHENTICATED root router), or split across lines was
// MISSED — and this lane's own header says a re-opened bare route is a CROSS-TENANT READ of every
// workspace's batch prompts, not merely an unbilled one. Registrations now come from the AST.
func TestBatchLane_NoBareRouteRegistration(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	w, err := scanBatchWiring("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if len(w.bare) > 0 {
		var got []string
		for _, r := range w.bare {
			got = append(got, fmt.Sprintf("%s.%s(%q) at line %d", r.receiver, r.verb, r.path, r.line))
		}
		t.Fatalf("batch routes registered WITHOUT the gate: %s\n"+
			"register through batchGate.{get,post} so the lane cannot be reached while it bills nothing",
			strings.Join(got, ", "))
	}
}

// TestBatchLane_RegisteredThroughTheGate: the positive half — the routes still exist, through the
// gate. Without this the previous test passes by deleting the feature, which is a different change
// from switching it off.
//
// ⚠ AND A COMMENT COULD SUPPLY THE FEATURE UNTIL #522: it was a strings.Contains for the exact
// registration text, so deleting the gated registration and leaving that text in a commented line
// passed — precisely the "passes by deleting the feature" case the doc above says it prevents.
func TestBatchLane_RegisteredThroughTheGate(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	w, err := scanBatchWiring("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, want := range []struct{ verb, path string }{
		{"post", "/v1/batch/submit"},
		{"get", "/v1/batch/status/{requestID}"},
		{"get", "/v1/batch/jobs"},
	} {
		if _, ok := w.gatedRoute(want.verb, want.path); !ok {
			t.Errorf("missing gated registration: batchGate.%s(authed, %q, …) — found %v",
				want.verb, want.path, w.gated)
		}
	}
}

// TestBatchLane_DefaultsOff: an unbilled lane must be off unless somebody consciously turns it on.
// A default-on switch would leave the deployment exactly where it started.
func TestBatchLane_DefaultsOff(t *testing.T) {
	setRequiredEnv(t)
	_ = os.Unsetenv("LENS_BATCH_ENABLED")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BatchEnabled {
		t.Fatal("LENS_BATCH_ENABLED defaults ON — an unbilled lane must default off")
	}
}

// TestBatchLane_FlagAloneCannotOpenAnUnbilledLane is the money invariant, stated as the only thing
// that actually protects the ledger: the operator's intent is not evidence of billing.
//
// ⚠ THIS IS THE TEST THAT WOULD HAVE CAUGHT THE ORIGINAL SHIP. The lane's own comment already
// described the gap; what was missing was something that could REFUSE. Turning the flag on with no
// settle hook must leave the route unreachable, not merely logged about.
func TestBatchLane_FlagAloneCannotOpenAnUnbilledLane(t *testing.T) {
	if g := newBatchReg(true, false); g.on {
		t.Fatal("the lane opened on the flag alone, with no settle hook wired — " +
			"every completed job would debit nothing while Talyvor pays the provider")
	}
	// And the converse, so the gate is not simply always-closed: with a settle wired, the
	// operator's switch is honoured. A guard that can never open is indistinguishable from
	// deleting the feature, which is a different decision.
	if g := newBatchReg(true, true); !g.on {
		t.Fatal("a wired settle plus an explicit flag must open the lane")
	}
	if g := newBatchReg(false, true); g.on {
		t.Fatal("the lane opened without the operator asking for it")
	}
}

// TestBatchLane_GateActuallyWithholdsTheRoute: the gate is only worth having if a closed gate
// leaves chi with nothing registered — asserted on a real router rather than on the struct field.
func TestBatchLane_GateActuallyWithholdsTheRoute(t *testing.T) {
	closed := newBatchReg(true, false) // asked for, unbilled ⇒ refused
	r := chi.NewRouter()
	closed.post(r, "/v1/batch/submit", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the handler ran — a closed gate must not register the route")
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/batch/submit", nil))
	// Asserting the HANDLER never ran (above) is the real claim; the 404 is chi's, and it is the
	// same 404 a path that never existed returns — no existence oracle for a closed lane.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("closed gate served %d — want chi's native 404, indistinguishable from absent", rec.Code)
	}
}
