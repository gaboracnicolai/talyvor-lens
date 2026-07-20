package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/workspace"
)

type fakeRoster struct {
	byID map[string]*workspace.Workspace
	all  []*workspace.Workspace
}

func (f *fakeRoster) GetWorkspace(id string) (*workspace.Workspace, bool) {
	ws, ok := f.byID[id]
	return ws, ok
}
func (f *fakeRoster) ListWorkspaces() []*workspace.Workspace { return f.all }

// GET /v1/workspaces: self-scoped, and MUST be a JSON ARRAY from day one (a single element today — the
// credential maps to one workspace) so a future multi-workspace token widens it with no contract change.
func TestListMyWorkspaces_ArrayScopedToCaller(t *testing.T) {
	roster := &fakeRoster{byID: map[string]*workspace.Workspace{
		"wsA": {ID: "wsA", Name: "Alpha"},
		"wsB": {ID: "wsB", Name: "Beta"},
	}}
	h := newListMyWorkspacesHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "wsA"}}, roster)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, "[") {
		t.Fatalf("response MUST be a JSON array from day one (single element under auth), got: %s", body)
	}
	var got []workspace.Workspace
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("not a JSON array: %v (body=%s)", err, body)
	}
	if len(got) != 1 || got[0].ID != "wsA" {
		t.Fatalf("must be exactly [wsA], got %+v", got)
	}
	if strings.Contains(body, "wsB") || strings.Contains(body, "Beta") {
		t.Errorf("leaked another workspace: %s", body)
	}
}

// A caller with no resolved workspace → 401 (fail closed).
func TestListMyWorkspaces_Unauthenticated401(t *testing.T) {
	h := newListMyWorkspacesHandler(fakeAuthn{ctx: &auth.AuthContext{}}, &fakeRoster{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-workspace caller: status=%d, want 401", rec.Code)
	}
}

// A caller whose workspace isn't in the roster → an EMPTY array (200 [] , never null), still an array.
func TestListMyWorkspaces_UnknownWorkspaceEmptyArray(t *testing.T) {
	h := newListMyWorkspacesHandler(fakeAuthn{ctx: &auth.AuthContext{WorkspaceID: "ghost"}}, &fakeRoster{byID: map[string]*workspace.Workspace{}})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("unknown workspace must be an empty array [], got: %s", rec.Body.String())
	}
}

// GET /v1/admin/workspaces (requireAdmin-gated at the mount site) returns the full roster as an array.
func TestAdminListWorkspaces_ReturnsAll(t *testing.T) {
	roster := &fakeRoster{all: []*workspace.Workspace{{ID: "wsA"}, {ID: "wsB"}}}
	h := newAdminListWorkspacesHandler(roster)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/workspaces", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var got []workspace.Workspace
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("not a JSON array: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("admin roster: got %d, want 2", len(got))
	}
}
