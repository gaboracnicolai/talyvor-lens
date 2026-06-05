package workspace

import (
	"context"
	"testing"
)

// DistillPolicy mirrors LoggingPolicy: default-OFF so the request-path
// distillation is inert until an admin enables a workspace.
func TestDistillPolicy_DefaultDisabled(t *testing.T) {
	m := New(nil)
	if got := m.GetDistillPolicy("unknown-ws"); got != DistillDisabled {
		t.Errorf("unknown workspace must be DistillDisabled (inert); got %q", got)
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})
	if got := m.GetDistillPolicy("w"); got != DistillDisabled {
		t.Errorf("a workspace with no policy set must default to DistillDisabled; got %q", got)
	}
}

func TestNormalizeDistillPolicy(t *testing.T) {
	for _, p := range []DistillPolicy{DistillOptIn, DistillAlways, DistillDisabled} {
		if normalizeDistillPolicy(p) != p {
			t.Errorf("valid %q should pass through", p)
		}
	}
	if normalizeDistillPolicy("") != DistillDisabled || normalizeDistillPolicy("bogus") != DistillDisabled {
		t.Error("unknown/empty must normalize to DistillDisabled (safe/off)")
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
