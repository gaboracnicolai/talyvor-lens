package workspace

import (
	"context"
	"testing"
)

// THE DEFAULT IS THE FEATURE. Pinned against the LITERAL "disabled" rather than
// against CompressionDisabled, because comparing a constant to itself passes for
// every value the constant could take. Every workspace that exists today set no
// policy, so this constant is what decides whether their prompts are rewritten.
func TestCompressionPolicy_DefaultIsTheLiteralDisabled(t *testing.T) {
	if string(DefaultCompressionPolicy) != "disabled" {
		t.Fatalf("DefaultCompressionPolicy = %q, want the literal \"disabled\" — the prompt rewriter measured 0.000%% on 308 corpus prompts and rewrote 8 of 8 agent-traffic ones; turning it on by default needs a decision, not an edit", DefaultCompressionPolicy)
	}
	// The two normalize branches below (unset vs garbage) are indistinguishable
	// while this holds. When the default moves, they stop being — and the
	// fail-safe branch starts carrying weight.
	if DefaultCompressionPolicy != CompressionDisabled {
		t.Error("DefaultCompressionPolicy and CompressionDisabled have diverged: normalizeCompressionPolicy's empty and garbage branches are now separately observable and each needs its own test")
	}
}

// An unregistered workspace, and a registered one that set nothing, both answer
// "do not rewrite". The unregistered case is the one the proxy hits for transient
// or header-supplied workspace ids.
func TestCompressionPolicy_UnknownAndUnsetAreBothOff(t *testing.T) {
	m := New(nil)
	if got := m.GetCompressionPolicy("unknown-ws"); got != CompressionDisabled {
		t.Errorf("unknown workspace must fail safe to disabled; got %q", got)
	}
	if err := m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true}); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCompressionPolicy("w"); got != CompressionDisabled {
		t.Errorf("a registered workspace with no policy must be disabled; got %q", got)
	}
}

func TestNormalizeCompressionPolicy(t *testing.T) {
	for _, p := range []CompressionPolicy{CompressionOptIn, CompressionAlways, CompressionDisabled} {
		if normalizeCompressionPolicy(p) != p {
			t.Errorf("valid %q should pass through", p)
		}
	}
	// ⚠ THIS ASSERTION IS BLIND TO THE VALUE OF THE CONSTANT, and that is measured,
	// not suspected: flipping DefaultCompressionPolicy to CompressionAlways leaves
	// it green (control C3). It compares the function's answer to the same constant
	// the function returns, so it passes for every value the constant could take.
	// It is kept because it is not vacuous — it still catches a `case "":` branch
	// that stops returning the default at all — but the guard that would speak if
	// the default moved is TestCompressionPolicy_DefaultIsTheLiteralDisabled above,
	// and the ones that speak at the wire are TestUpstream_DefaultWorkspaceSendsThe
	// PromptUnchanged and TestCompressionPolicy_SurvivesRestart_RealPG.
	if got := normalizeCompressionPolicy(""); got != DefaultCompressionPolicy {
		t.Errorf("empty/unset must resolve to DefaultCompressionPolicy (%q); got %q", DefaultCompressionPolicy, got)
	}
	if got := normalizeCompressionPolicy("bogus"); got != CompressionDisabled {
		t.Errorf("garbage must fail SAFE to disabled; got %q", got)
	}
}

// An explicit opt-IN survives registration and a re-read — otherwise the policy
// would be decoration.
func TestCompressionPolicy_ExplicitOptInPreserved(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "on", Name: "On", Active: true, CompressionPolicy: CompressionAlways})
	if got := m.GetCompressionPolicy("on"); got != CompressionAlways {
		t.Errorf("an explicit CompressionAlways must be preserved; got %q", got)
	}
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "hdr", Name: "Hdr", Active: true, CompressionPolicy: CompressionOptIn})
	if got := m.GetCompressionPolicy("hdr"); got != CompressionOptIn {
		t.Errorf("an explicit CompressionOptIn must be preserved; got %q", got)
	}
	// And an invalid stored value normalizes OFF at registration, not at read time
	// only — a truncated write must not leave a workspace rewriting prompts.
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "bad", Name: "Bad", Active: true, CompressionPolicy: "alway"})
	if got := m.GetCompressionPolicy("bad"); got != CompressionDisabled {
		t.Errorf("a near-miss typo must normalize to disabled; got %q", got)
	}
}

func TestSetCompressionPolicy_InMemory(t *testing.T) {
	m := New(nil)
	_ = m.RegisterWorkspace(context.Background(), Workspace{ID: "w", Name: "W", Active: true})

	if err := m.SetCompressionPolicy(context.Background(), "w", CompressionOptIn); err != nil {
		t.Fatal(err)
	}
	if got := m.GetCompressionPolicy("w"); got != CompressionOptIn {
		t.Errorf("after Set, GetCompressionPolicy = %q want opt_in", got)
	}
	_ = m.SetCompressionPolicy(context.Background(), "w", CompressionPolicy("garbage"))
	if got := m.GetCompressionPolicy("w"); got != CompressionDisabled {
		t.Errorf("garbage policy must normalize to disabled; got %q", got)
	}
	if err := m.SetCompressionPolicy(context.Background(), "nope", CompressionAlways); err == nil {
		t.Error("setting policy on an unregistered workspace should error")
	}
}
