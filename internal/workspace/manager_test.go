package workspace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newManagerForTest(t *testing.T) *Manager {
	t.Helper()
	return New(nil)
}

func TestRegisterWorkspace_GeneratesCachePrefixWhenEmpty(t *testing.T) {
	m := newManagerForTest(t)

	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "Team A", Active: true,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}

	ws, ok := m.GetWorkspace("team-a")
	if !ok || ws == nil {
		t.Fatal("workspace not stored")
	}
	if ws.CachePrefix != "ws:team-a:" {
		t.Errorf("CachePrefix = %q, want %q", ws.CachePrefix, "ws:team-a:")
	}
}

func TestRegisterWorkspace_RejectsEmptyID(t *testing.T) {
	m := newManagerForTest(t)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		Name: "no id", Active: true,
	}); err == nil {
		t.Fatal("expected error for empty ID; got nil")
	}
}

func TestGetWorkspace_NilForUnknown(t *testing.T) {
	m := newManagerForTest(t)
	got, ok := m.GetWorkspace("nope")
	if ok || got != nil {
		t.Errorf("GetWorkspace = (%v, %v), want (nil, false)", got, ok)
	}
}

func TestExtractWorkspaceID_ReadsHeader(t *testing.T) {
	m := newManagerForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Talyvor-Workspace", "team-a")
	if got := m.ExtractWorkspaceID(req); got != "team-a" {
		t.Errorf("ExtractWorkspaceID = %q, want %q", got, "team-a")
	}
}

func TestExtractWorkspaceID_DefaultWhenMissing(t *testing.T) {
	m := newManagerForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := m.ExtractWorkspaceID(req); got != "default" {
		t.Errorf("ExtractWorkspaceID = %q, want %q", got, "default")
	}
}

func TestCheckPolicy_AllowsWhenNoRestrictions(t *testing.T) {
	m := newManagerForTest(t)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "A", Active: true,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}

	policy := m.CheckPolicy(context.Background(), "team-a", "openai", "gpt-4o", 1000)

	if !policy.Allowed {
		t.Errorf("Allowed = false, want true (violation: %q)", policy.Violation)
	}
	if policy.Violation != "" {
		t.Errorf("Violation = %q, want empty", policy.Violation)
	}
}

func TestCheckPolicy_BlocksDisallowedProvider(t *testing.T) {
	m := newManagerForTest(t)
	_ = m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "A", Active: true,
		AllowedProviders: []string{"anthropic"},
	})

	policy := m.CheckPolicy(context.Background(), "team-a", "openai", "gpt-4o", 100)

	if policy.Allowed {
		t.Errorf("Allowed = true, want false (openai not in allowed list)")
	}
	if policy.Violation == "" {
		t.Error("Violation should be non-empty when blocked")
	}
}

func TestCheckPolicy_BlocksDisallowedModel(t *testing.T) {
	m := newManagerForTest(t)
	_ = m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "A", Active: true,
		AllowedModels: []string{"gpt-4o-mini"},
	})

	policy := m.CheckPolicy(context.Background(), "team-a", "openai", "gpt-4o", 100)

	if policy.Allowed {
		t.Errorf("Allowed = true, want false (gpt-4o not in allowed list)")
	}
	if policy.Violation == "" {
		t.Error("Violation should be non-empty when blocked")
	}
}

func TestScopedCacheKey_PrefixesCorrectly(t *testing.T) {
	m := newManagerForTest(t)
	_ = m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "A", Active: true,
	})

	got := m.ScopedCacheKey("team-a", "some-key")
	want := "ws:team-a:some-key"
	if got != want {
		t.Errorf("ScopedCacheKey = %q, want %q", got, want)
	}
}

func TestCheckPolicy_AllowsWhenModelInAllowedList(t *testing.T) {
	m := newManagerForTest(t)
	_ = m.RegisterWorkspace(context.Background(), Workspace{
		ID: "team-a", Name: "A", Active: true,
		AllowedModels: []string{"gpt-4o", "gpt-4o-mini"},
	})

	policy := m.CheckPolicy(context.Background(), "team-a", "openai", "gpt-4o", 100)

	if !policy.Allowed {
		t.Errorf("Allowed = false, want true (gpt-4o is in allowed list); violation: %q", policy.Violation)
	}
}
