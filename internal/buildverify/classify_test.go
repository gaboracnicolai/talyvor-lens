package buildverify

import "testing"

// FIX #1 — a Go toolchain INTERNAL ERROR / crash / panic must NEVER be compile_failed (that would be a FALSE
// SLASH in step 3). Only a clean diagnostic-style failure is compile_failed; everything else → not_verifiable.
func TestClassifyBuildResult_ICEIsNotVerifiable(t *testing.T) {
	cases := []struct {
		name   string
		exit   int
		output string
		want   Verdict
	}{
		{"clean compile", 0, "", Compiled},
		{"clean type error", 1, "./main.go:6:14: cannot use \"x\" (untyped string) as int value", CompileFailed},
		{"clean syntax error exit2", 2, "./main.go:3:1: expected declaration, found '}'", CompileFailed},
		// crashes / ICEs — must refuse, never compile_failed:
		{"internal compiler error", 2, "./main.go:1: internal compiler error: bad thing\n\ngoroutine 1 [running]:", NotVerifiable},
		{"compiler panic", 2, "panic: runtime error: invalid memory address\n\ngoroutine 17:", NotVerifiable},
		{"SIGSEGV", 2, "fatal error: unexpected signal during runtime execution\n[signal SIGSEGV: segmentation violation", NotVerifiable},
		{"go runtime fatal", 2, "fatal error: out of memory", NotVerifiable},
		{"OOM kill", 137, "", NotVerifiable},
		{"weird nonzero, no diagnostic", 1, "some non-diagnostic gibberish with no file:line", NotVerifiable},
		{"unusual exit code", 127, "not found", NotVerifiable},
	}
	for _, c := range cases {
		if got := classifyBuildResult(c.exit, c.output); got != c.want {
			t.Errorf("%s: classifyBuildResult(%d, %q)=%q want %q", c.name, c.exit, c.output, got, c.want)
		}
	}
}

func TestSplitPlatform(t *testing.T) {
	if g, a, ok := splitPlatform("linux/amd64"); !ok || g != "linux" || a != "amd64" {
		t.Errorf("splitPlatform(linux/amd64)=%q,%q,%v", g, a, ok)
	}
	if _, _, ok := splitPlatform("garbage"); ok {
		t.Error("splitPlatform(garbage) must fail")
	}
}
