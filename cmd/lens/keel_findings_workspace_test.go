package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/keel"
)

// (fakeKeelLister is defined in keel_handler_test.go — extended there with ListFindingsForWorkspace.)

// SEC-4/SEC-5: the tenant read is scoped to the AUTHENTICATED workspace, and a HOSTILE caller-supplied
// workspace_id (query OR the URL) is IGNORED — the request is never parsed for one.
func TestKeelFindings_WorkspaceScoped_HostileParamIgnored(t *testing.T) {
	lister := &fakeKeelLister{byWorkspace: map[string][]keel.ListedFinding{
		"ws-alpha": {{WorkspaceID: "ws-alpha", Unit: "model_used", Attribution: "idiosyncratic", Mode: "hardened"}},
		"ws-beta":  {{WorkspaceID: "ws-beta", Unit: "model_used", Attribution: "idiosyncratic", Mode: "hardened"}},
	}}
	h := newKeelFindingsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-alpha"}}, lister)
	rec := httptest.NewRecorder()
	// hostile: caller is ws-alpha but asks for ws-beta via a param.
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/keel/findings?workspace_id=ws-beta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if lister.wsCalledWith != "ws-alpha" {
		t.Errorf("scope must come from AUTH, not the param; ListFindingsForWorkspace called with %q, want ws-alpha", lister.wsCalledWith)
	}
	if strings.Contains(rec.Body.String(), "ws-beta") {
		t.Errorf("SEC-4/5 BREACH: another tenant's rows leaked: %s", rec.Body.String())
	}
}

// Unauthenticated (no resolved workspace) → 401; the lister is never reached.
func TestKeelFindings_Unauthenticated_401(t *testing.T) {
	lister := &fakeKeelLister{}
	h := newKeelFindingsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{}}, lister)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/keel/findings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-workspace caller: status=%d, want 401", rec.Code)
	}
	if lister.wsCalledWith != "" {
		t.Error("unauthenticated: the reader must NOT be reached")
	}
}

// Ordinary + hardened rows for the same (ws,unit,window) both return, each correctly labeled, never conflated.
func TestKeelFindings_OrdinaryAndHardened_BothLabeled(t *testing.T) {
	lister := &fakeKeelLister{byWorkspace: map[string][]keel.ListedFinding{
		"ws-alpha": {
			{WorkspaceID: "ws-alpha", Unit: "model_used", Window: 100, Attribution: "idiosyncratic", Mode: "hardened"},
			{WorkspaceID: "ws-alpha", Unit: "model_used", Window: 100, Attribution: "common_mode", Mode: "ordinary"},
		},
	}}
	h := newKeelFindingsWorkspaceHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ws-alpha"}}, lister)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/keel/findings", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"mode":"hardened"`) || !strings.Contains(body, `"mode":"ordinary"`) {
		t.Errorf("both modes must be present + labeled; body=%s", body)
	}
	if !strings.Contains(body, `"attribution":"idiosyncratic"`) || !strings.Contains(body, `"attribution":"common_mode"`) {
		t.Errorf("both attributions must be present + labeled; body=%s", body)
	}
}
