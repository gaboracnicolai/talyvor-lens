package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/talyvor/lens/internal/attribution"
)

// authz_attribution_test.go — #151. The two repo/branch reads must scope to the
// caller's workspace: a tenant sees only its OWN slice of a (possibly shared)
// repository name. Behavioral, over HTTP against the real handlers, asserting
// the workspace passed to the Store dependency.

type fakeBranchSpendReader struct {
	gotWorkspace string
	gotRepo      string
	branch       *attribution.BranchSpend
	top          []attribution.BranchSpend
}

func (f *fakeBranchSpendReader) GetBranchSpendForWorkspace(_ context.Context, ws, _, repo string) (*attribution.BranchSpend, error) {
	f.gotWorkspace, f.gotRepo = ws, repo
	return f.branch, nil
}

func (f *fakeBranchSpendReader) GetTopBranchesForWorkspace(_ context.Context, ws, repo string, _ int) ([]attribution.BranchSpend, error) {
	f.gotWorkspace, f.gotRepo = ws, repo
	return f.top, nil
}

func TestAttributionBranch_WorkspaceScoped(t *testing.T) {
	f := &fakeBranchSpendReader{branch: &attribution.BranchSpend{Branch: "b", Repository: "R", TotalCostUSD: 10}}
	h := newAttributionBranchHandler(f)

	// ATTACK: non-admin wsA passes ?workspace_id=wsB → handler must query wsA's
	// slice (the param ignored). wsA can never see wsB's spend for repo R.
	serveAuthed(t, http.MethodGet, "/v1/attribution/branch", "/v1/attribution/branch?repository=R&branch=b&workspace_id=wsB", "", "wsA", false, h)
	if f.gotWorkspace != "wsA" {
		t.Fatalf("LEAK: branch scoped to %q, want wsA (the ?workspace_id=wsB must be ignored)", f.gotWorkspace)
	}
	// no param → own workspace.
	f.gotWorkspace = ""
	serveAuthed(t, http.MethodGet, "/v1/attribution/branch", "/v1/attribution/branch?repository=R&branch=b", "", "wsA", false, h)
	if f.gotWorkspace != "wsA" {
		t.Fatalf("no-param non-admin scoped to %q, want wsA", f.gotWorkspace)
	}
	// admin honors the param.
	f.gotWorkspace = ""
	serveAuthed(t, http.MethodGet, "/v1/attribution/branch", "/v1/attribution/branch?repository=R&branch=b&workspace_id=wsB", "", "ws-adm", true, h)
	if f.gotWorkspace != "wsB" {
		t.Fatalf("admin scoped to %q, want wsB (honored)", f.gotWorkspace)
	}
	// empty result → 404 (unchanged).
	f.branch = nil
	if rec := serveAuthed(t, http.MethodGet, "/v1/attribution/branch", "/v1/attribution/branch?repository=R&branch=b", "", "wsA", false, h); rec.Code != http.StatusNotFound {
		t.Fatalf("nil branch: got %d, want 404", rec.Code)
	}
}

func TestAttributionTop_WorkspaceScoped(t *testing.T) {
	f := &fakeBranchSpendReader{top: []attribution.BranchSpend{{Branch: "b", Repository: "R"}}}
	h := newAttributionTopHandler(f)

	// non-admin wsA + ?workspace_id=wsB → scoped to wsA.
	serveAuthed(t, http.MethodGet, "/v1/attribution/top", "/v1/attribution/top?repository=R&workspace_id=wsB", "", "wsA", false, h)
	if f.gotWorkspace != "wsA" {
		t.Fatalf("LEAK: top scoped to %q, want wsA", f.gotWorkspace)
	}
	// admin honors the param.
	f.gotWorkspace = ""
	serveAuthed(t, http.MethodGet, "/v1/attribution/top", "/v1/attribution/top?repository=R&workspace_id=wsB", "", "ws-adm", true, h)
	if f.gotWorkspace != "wsB" {
		t.Fatalf("admin top scoped to %q, want wsB", f.gotWorkspace)
	}
}
