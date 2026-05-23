package workspace

import (
	"context"
	"testing"
)

func TestGetLoggingPolicy_DefaultsToMetadataForUnknownWorkspace(t *testing.T) {
	m := New(nil)
	got := m.GetLoggingPolicy("ws-unknown")
	if got != LoggingMetadata {
		t.Errorf("GetLoggingPolicy(unknown) = %q, want %q", got, LoggingMetadata)
	}
}

func TestGetLoggingPolicy_ReturnsRegisteredPolicy(t *testing.T) {
	m := New(nil)
	cases := []struct {
		policy LoggingPolicy
		wsID   string
	}{
		{LoggingFull, "ws-full"},
		{LoggingMetadata, "ws-meta"},
		{LoggingNone, "ws-none"},
	}
	for _, tc := range cases {
		if err := m.RegisterWorkspace(context.Background(), Workspace{
			ID: tc.wsID, Name: "x", Active: true, LoggingPolicy: tc.policy,
		}); err != nil {
			t.Fatalf("RegisterWorkspace %s: %v", tc.wsID, err)
		}
		if got := m.GetLoggingPolicy(tc.wsID); got != tc.policy {
			t.Errorf("ws %s: GetLoggingPolicy = %q, want %q", tc.wsID, got, tc.policy)
		}
	}
}

func TestRegisterWorkspace_StoresLoggingPolicy(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "ws-1", Name: "team-1", Active: true, LoggingPolicy: LoggingNone,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	got, ok := m.GetWorkspace("ws-1")
	if !ok {
		t.Fatal("workspace not found after registration")
	}
	if got.LoggingPolicy != LoggingNone {
		t.Errorf("stored LoggingPolicy = %q, want %q", got.LoggingPolicy, LoggingNone)
	}
}

func TestRegisterWorkspace_DefaultsEmptyPolicyToMetadata(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "ws-default-policy", Name: "x", Active: true,
		// LoggingPolicy intentionally omitted — should fall back.
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if got := m.GetLoggingPolicy("ws-default-policy"); got != LoggingMetadata {
		t.Errorf("default LoggingPolicy = %q, want %q", got, LoggingMetadata)
	}
}

func TestSetLoggingPolicy_UpdatesInMemoryPolicy(t *testing.T) {
	m := New(nil)
	if err := m.RegisterWorkspace(context.Background(), Workspace{
		ID: "ws-x", Name: "x", Active: true, LoggingPolicy: LoggingFull,
	}); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if err := m.SetLoggingPolicy(context.Background(), "ws-x", LoggingNone); err != nil {
		t.Fatalf("SetLoggingPolicy: %v", err)
	}
	if got := m.GetLoggingPolicy("ws-x"); got != LoggingNone {
		t.Errorf("after SetLoggingPolicy(none), GetLoggingPolicy = %q, want %q", got, LoggingNone)
	}
}

func TestSetLoggingPolicy_UnknownWorkspaceErrors(t *testing.T) {
	m := New(nil)
	if err := m.SetLoggingPolicy(context.Background(), "ws-nope", LoggingFull); err == nil {
		t.Error("SetLoggingPolicy on unknown workspace should error")
	}
}

func TestNormalizeLoggingPolicy_GarbagePolicyDefaultsSafe(t *testing.T) {
	// A schema bug or a hand-edited DB row could deposit a bogus string
	// in logging_policy. We want the safe default, not "full".
	if got := normalizeLoggingPolicy(LoggingPolicy("verbose")); got != LoggingMetadata {
		t.Errorf("normalize garbage = %q, want %q", got, LoggingMetadata)
	}
	if got := normalizeLoggingPolicy(""); got != LoggingMetadata {
		t.Errorf("normalize empty = %q, want %q", got, LoggingMetadata)
	}
}
