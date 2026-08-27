package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/batch"
)

// batch_submit_handler_test.go — the workspace POST /v1/batch/submit submits under.
//
// ⚠ THE LANE IS CLOSED IN EVERY CONFIGURATION and these tests do not pretend
// otherwise — TestBatchLane_CannotOpenWhileTheJobListIsUnscoped below asserts the
// closure from main.go's own call site. They drive the handler directly, which is
// the only way to reach it, and that is the point: the property has to hold BEFORE
// somebody opens the door, not after.

type fakeBatchSubmitter struct {
	gotWS, gotModel, gotPrompt string
	eligible                   bool
	submits                    int
}

func (f *fakeBatchSubmitter) IsEligible(_ []byte, wsID string) batch.BatchEligibility {
	f.gotWS = wsID
	if !f.eligible {
		return batch.BatchEligibility{Eligible: false, Reason: "not eligible"}
	}
	return batch.BatchEligibility{Eligible: true}
}

func (f *fakeBatchSubmitter) Submit(_ context.Context, wsID, model, prompt string, _ []byte) (*batch.BatchJob, error) {
	f.gotWS, f.gotModel, f.gotPrompt = wsID, model, prompt
	f.submits++
	return &batch.BatchJob{ID: "b1", RequestID: "r1", WorkspaceID: wsID, Status: batch.BatchPending}, nil
}

const batchBody = `{"model":"claude-3-haiku","messages":[{"content":"hello"}]}`

func TestBatchSubmit_HeaderCannotNameAnotherWorkspace(t *testing.T) {
	f := &fakeBatchSubmitter{eligible: true}
	h := newBatchSubmitHandler(f)

	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "ws-attacker", false)
	req.Header.Set("X-Talyvor-Workspace", "ws-victim")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	// NON-VACUITY: a job really was submitted, so "not under ws-victim" cannot be
	// true merely because nothing happened.
	if f.submits != 1 {
		t.Fatalf("Submit was called %d times, want 1", f.submits)
	}
	if f.gotWS != "ws-attacker" {
		t.Errorf("a ws-attacker credential submitted batch work under workspace %q, because the "+
			"handler took it from the X-Talyvor-Workspace HEADER. A header is a request, not an "+
			"identity — #146 already decided that for every other non-{wsID} route.", f.gotWS)
	}
}

func TestBatchSubmit_MissingHeaderDoesNotLandInASharedDefaultBucket(t *testing.T) {
	f := &fakeBatchSubmitter{eligible: true}
	h := newBatchSubmitHandler(f)

	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "ws-me", false)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if f.gotWS != "ws-me" {
		t.Errorf("a caller that sent no header submitted under %q, want its own workspace "+
			"ws-me — the old fallback put every unheadered tenant in one shared \"default\" "+
			"bucket, which is how tenants meet each other", f.gotWS)
	}
}

func TestBatchSubmit_AdminStillHonoursTheHeader(t *testing.T) {
	f := &fakeBatchSubmitter{eligible: true}
	h := newBatchSubmitHandler(f)

	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "", true)
	req.Header.Set("X-Talyvor-Workspace", "ws-any")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if f.gotWS != "ws-any" {
		t.Errorf("admin submit landed in %q, want ws-any — the operator's cross-workspace submit "+
			"was removed along with the tenant's", f.gotWS)
	}
}

func TestBatchSubmit_AdminWithNoHeaderKeepsTheDefaultBucket(t *testing.T) {
	// The global admin's empty value preserved the router's pre-existing
	// "default" behaviour. Narrowing the tenant case must not change the operator's.
	f := &fakeBatchSubmitter{eligible: true}
	h := newBatchSubmitHandler(f)

	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "", true)
	rec := httptest.NewRecorder()
	h(rec, req)

	if f.gotWS != "default" {
		t.Errorf("admin with no header submitted under %q, want \"default\"", f.gotWS)
	}
}

func TestBatchSubmit_NoWorkspaceIdentityIsRefused(t *testing.T) {
	f := &fakeBatchSubmitter{eligible: true}
	h := newBatchSubmitHandler(f)

	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "", false)
	req.Header.Set("X-Talyvor-Workspace", "ws-victim")
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("a non-admin with no resolvable workspace: status = %d, want 403; it must not "+
			"fall through to the header or to \"default\"", rec.Code)
	}
	if f.submits != 0 {
		t.Errorf("Submit ran %d times for a refused caller, want 0", f.submits)
	}
}

func TestBatchSubmit_IneligibleBodyIsStillRefused(t *testing.T) {
	f := &fakeBatchSubmitter{eligible: false}
	h := newBatchSubmitHandler(f)
	req := authedReq(t, http.MethodPost, "/v1/batch/submit", batchBody, "ws-me", false)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("ineligible body: status = %d, want 400 — the eligibility gate must still run", rec.Code)
	}
}

// ── the wiring ──

func TestBatchSubmitRouteGoesThroughTheScopedHandler(t *testing.T) {
	src := readMainGo(t)
	const want = `batchGate.post(authed, "/v1/batch/submit", newBatchSubmitHandler(batchRouter))`
	if !strings.Contains(src, want) {
		t.Errorf("main.go does not register POST /v1/batch/submit through newBatchSubmitHandler")
	}
	if strings.Contains(src, `wsID := req.Header.Get("X-Talyvor-Workspace")`) {
		t.Error(`main.go still derives a workspace from the X-Talyvor-Workspace header inline. ` +
			`A header is a request, not an identity.`)
	}
}

// ⚠ THE COUPLING GUARD, AND THE HALF THIS MERGE DOES NOT FIX.
//
// GET /v1/batch/jobs returns batchRouter.ListJobs(), which takes no workspace and
// says so in its own comment: "workspace_id filtering happens client-side for now
// — the in-memory list doesn't index by workspace". batch.BatchJob carries
// WorkspaceID AND Prompt, so an open lane would hand every tenant every other
// tenant's prompt text. Fixing that means indexing the pending map by workspace
// inside internal/batch — a change to a data structure, not a line at the edge,
// and its own item.
//
// What IS a session's to do is make the two facts inseparable: the lane may stay
// closed with an unscoped list, and it may open with a scoped one, and it may not
// open with an unscoped one. This fails the moment somebody changes main.go's
// literal `false` without having given ListJobs a workspace argument first.
func TestBatchLane_CannotOpenWhileTheJobListIsUnscoped(t *testing.T) {
	src := readMainGo(t)
	const closed = `batchGate := newBatchReg(cfg.BatchEnabled, false)`

	raw, err := os.ReadFile("../../internal/batch/router.go")
	if err != nil {
		t.Fatalf("read internal/batch/router.go: %v", err)
	}
	routerSrc := string(raw)
	listScoped := !strings.Contains(routerSrc, "func (r *BatchRouter) ListJobs() []*BatchJob")

	// Non-vacuity: the two things this couples must both be findable.
	if !strings.Contains(routerSrc, "ListJobs(") {
		t.Fatal("internal/batch/router.go has no ListJobs — this guard is coupling two facts and " +
			"one of them has vanished, so it proves nothing")
	}

	if strings.Contains(src, closed) {
		// Lane closed. Nothing to enforce, but say what is being carried.
		if !listScoped {
			t.Log("batch lane is CLOSED (settleWired literal false) and ListJobs is unscoped — " +
				"latent, and this guard is what stops those two facts separating")
		}
		return
	}
	if !listScoped {
		t.Error("main.go no longer passes a literal false for settleWired, so the /v1/batch lane " +
			"can now be REGISTERED — and BatchRouter.ListJobs still takes no workspace. " +
			"GET /v1/batch/jobs would hand every tenant every other tenant's job, including its " +
			"Prompt. Scope ListJobs before opening the lane.")
	}
}
