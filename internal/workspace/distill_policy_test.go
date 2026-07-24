package workspace

import (
	"context"
	"testing"
)

// Distill is ON by default (DefaultDistillPolicy = DistillAlways): a registered
// workspace that sets no policy distills its documents. It is the one saving that
// reaches the customer's charge (it shrinks the document BEFORE the pre-serve
// estimate) and it degrades safely to today's behaviour when no worker binary is
// configured (a conversion failure never fails a request — proxy MaybeDistill).
// An UNKNOWN (unregistered) workspace and a stale read still fail SAFE to
// DistillDisabled — only a real, registered, unconfigured workspace opts in.
func TestDistillPolicy_DefaultAlways(t *testing.T) {
	m := New(nil)
	if got := m.GetDistillPolicy("unknown-ws"); got != DistillDisabled {
		t.Errorf("unknown workspace must fail safe to DistillDisabled (inert); got %q", got)
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if got := m.GetDistillPolicy("w"); got != DistillAlways {
		t.Errorf("a registered workspace with no policy must default to DistillAlways (on by default); got %q", got)
	}
}

func TestNormalizeDistillPolicy(t *testing.T) {
	for _, p := range []DistillPolicy{DistillOptIn, DistillAlways, DistillDisabled} {
		if normalizeDistillPolicy(p) != p {
			t.Errorf("valid %q should pass through", p)
		}
	}
	// EMPTY (unset — the workspace wants the default) resolves to the on-by-default
	// policy; GARBAGE (a typo) still fails SAFE to disabled so a misconfiguration
	// never silently distills.
	if got := normalizeDistillPolicy(""); got != DefaultDistillPolicy {
		t.Errorf("empty/unset must resolve to DefaultDistillPolicy (%q); got %q", DefaultDistillPolicy, got)
	}
	if got := normalizeDistillPolicy("bogus"); got != DistillDisabled {
		t.Errorf("garbage must fail SAFE to DistillDisabled; got %q", got)
	}
}

// An explicit opt-OUT (DistillDisabled) is honoured, never flipped by the new
// default — the guarantee that existing workspaces (stored 'disabled') stay off.
func TestDistillPolicy_ExplicitDisabledHonoured(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true, DistillPolicy: DistillDisabled})
	if got := m.GetDistillPolicy("w"); got != DistillDisabled {
		t.Errorf("an explicit DistillDisabled must be preserved (existing workspaces stay off); got %q", got)
	}
}

func TestSetDistillPolicy_InMemory(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	if err := m.SetDistillPolicy(context.Background(), "w", DistillOptIn); err != nil {
		t.Fatal(err)
	}
	if got := m.GetDistillPolicy("w"); got != DistillOptIn {
		t.Errorf("after Set, GetDistillPolicy = %q want opt_in", got)
	}
	// Garbage normalizes off.
	_ = m.SetDistillPolicy(context.Background(), "w", DistillPolicy("garbage"))
	if got := m.GetDistillPolicy("w"); got != DistillDisabled {
		t.Errorf("garbage policy must normalize to disabled; got %q", got)
	}
	// Setting an unregistered workspace errors.
	if err := m.SetDistillPolicy(context.Background(), "nope", DistillOptIn); err == nil {
		t.Error("setting policy on an unregistered workspace should error")
	}
}

func TestRegisterWorkspace_NormalizesDistillPolicy(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true, DistillPolicy: "weird"})
	if got := m.GetDistillPolicy("w"); got != DistillDisabled {
		t.Errorf("RegisterWorkspace must normalize an invalid DistillPolicy to disabled; got %q", got)
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w2", Name: "W2", Active: true, DistillPolicy: DistillAlways})
	if got := m.GetDistillPolicy("w2"); got != DistillAlways {
		t.Errorf("a valid DistillPolicy must be preserved; got %q", got)
	}
}
