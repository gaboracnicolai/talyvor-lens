package buildverify

import (
	"context"
	"testing"
)

// DISABLED (the default posture) → not_verifiable WITHOUT running anything.
func TestVerify_Disabled_RunsNothing(t *testing.T) {
	v := NewVerifier(false, WithDocker("/nonexistent/docker"))
	r := v.Verify(context.Background(), t.TempDir())
	if r.Verdict != NotVerifiable {
		t.Errorf("disabled verifier must return not_verifiable; got %q", r.Verdict)
	}
}

// FAIL CLOSED ON CONTAINMENT: enabled but no container runtime → not_verifiable, and it must NOT fall back to
// an unsandboxed exec. (Pointed at a bogus docker binary so dockerAvailable is false.)
func TestVerify_NoRuntime_FailsClosed(t *testing.T) {
	v := NewVerifier(true, WithDocker("/nonexistent/docker-binary"))
	r := v.Verify(context.Background(), t.TempDir())
	if r.Verdict != NotVerifiable {
		t.Fatalf("no runtime must yield not_verifiable; got %q", r.Verdict)
	}
	if r.Toolchain != "" {
		t.Errorf("no build should have run; toolchain must be empty, got %q", r.Toolchain)
	}
}

func TestMinorOf(t *testing.T) {
	for in, want := range map[string]int{"go1.25.11": 25, "go1.21": 21, "go1.26.4": 26, "garbage": 0} {
		if got := minorOf(in); got != want {
			t.Errorf("minorOf(%q)=%d want %d", in, got, want)
		}
	}
}
