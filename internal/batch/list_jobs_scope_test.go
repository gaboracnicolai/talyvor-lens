package batch

import "testing"

// list_jobs_scope_test.go — W6.38: GET /v1/batch/jobs must not hand one tenant another
// tenant's batch job.
//
// ⚠ WHY THIS IS A TENANCY TEST AND NOT A LIST TEST. BatchJob carries Prompt (the customer's
// text) and Response (the model's answer). ListJobs returned every job in the in-memory map,
// and cmd/lens wrote the result straight out, so an open lane would have handed every
// authenticated caller every other workspace's prompts.
//
// ⚠ THE DEFERRAL THIS REPLACES SAID THE FIX NEEDED A DATA-STRUCTURE CHANGE — "indexing the
// pending map by workspace inside internal/batch, a change to a data structure, not a line at
// the edge". It does not: BatchJob.WorkspaceID is set at Submit and has been all along, so a
// filter is exactly as correct as an index. An index is a PERFORMANCE choice over a map of
// pending jobs; the correctness question never needed one.

// seedJobs puts jobs for two workspaces into the pending map the way Submit does.
func seedJobs(t *testing.T) *BatchRouter {
	t.Helper()
	r := newBatchRouter(nil, "test-key")
	r.mu.Lock()
	r.pending["req-a1"] = &BatchJob{ID: "a1", WorkspaceID: "ws-a", RequestID: "req-a1", Prompt: "alpha secret"}
	r.pending["req-a2"] = &BatchJob{ID: "a2", WorkspaceID: "ws-a", RequestID: "req-a2", Prompt: "alpha second"}
	r.pending["req-b1"] = &BatchJob{ID: "b1", WorkspaceID: "ws-b", RequestID: "req-b1", Prompt: "beta secret"}
	r.mu.Unlock()
	return r
}

// TestListJobs_ReturnsOnlyTheCallersWorkspace is the tenancy invariant.
func TestListJobs_ReturnsOnlyTheCallersWorkspace(t *testing.T) {
	r := seedJobs(t)

	got := r.ListJobs("ws-a")
	if len(got) != 2 {
		t.Fatalf("ws-a sees %d jobs, want its own 2: %v", len(got), ids(got))
	}
	for _, j := range got {
		if j.WorkspaceID != "ws-a" {
			t.Errorf("ws-a was handed job %s owned by %s (prompt %q) — a tenant must never see "+
				"another tenant's batch prompt", j.ID, j.WorkspaceID, j.Prompt)
		}
	}

	// The positive control for the filter itself: the other tenant still sees its own job, so a
	// filter that returned nothing would not pass this file.
	if other := r.ListJobs("ws-b"); len(other) != 1 || other[0].ID != "b1" {
		t.Errorf("ws-b sees %v, want exactly its own b1 — the filter must scope, not empty", ids(other))
	}
}

// TestListJobs_UnknownAndEmptyWorkspaceSeeNothing — the two ways a caller could arrive without a
// workspace. Neither may be read as "show me everything", which is what an unfiltered list did.
func TestListJobs_UnknownAndEmptyWorkspaceSeeNothing(t *testing.T) {
	r := seedJobs(t)
	for _, ws := range []string{"ws-nobody", ""} {
		if got := r.ListJobs(ws); len(got) != 0 {
			t.Errorf("workspace %q sees %v, want nothing", ws, ids(got))
		}
	}
}

func ids(js []*BatchJob) []string {
	out := make([]string, 0, len(js))
	for _, j := range js {
		out = append(out, j.ID+"@"+j.WorkspaceID)
	}
	return out
}
