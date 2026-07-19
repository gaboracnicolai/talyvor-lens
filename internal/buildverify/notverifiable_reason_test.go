package buildverify

import (
	"strings"
	"testing"
)

// A module-unreadable/module-missing failure must be NAMED, not mislabeled as a toolchain crash. This is the
// exact failure mode of PR #325's CI red: a 0700 srcDir made the sandbox user unable to read the module, go
// failed with "directory prefix /src does not contain main module" (no file.go:N: diagnostic), and the
// generic reason blamed the compiler ("toolchain crash/ICE") — indistinguishable from a real ICE. The
// verdict stays NotVerifiable either way (fail-open unchanged); ONLY the reason string is sharpened so the
// next such bug names itself immediately.
func TestNotVerifiableReason_NamesUnreadableModule(t *testing.T) {
	unreadable := []string{
		"pattern ./...: directory prefix /src does not contain main module or its selected dependencies",
		"go: go.mod file not found in current directory or any parent directory",
		"pattern ./...: open /src: permission denied",
	}
	for _, out := range unreadable {
		r := notVerifiableReason("linux/amd64", out)
		if !strings.Contains(r, "module unreadable or missing in the sandbox") || !strings.Contains(r, "srcDir permissions") {
			t.Errorf("output %q must be named as a module-visibility failure, not a compiler crash; got %q", out, r)
		}
		if strings.Contains(r, "toolchain crash") {
			t.Errorf("output %q must NOT be labeled a toolchain crash; got %q", out, r)
		}
	}

	// A genuine compiler crash keeps the classic reason.
	ice := "compile: internal compiler error: assertion failed while compiling main.go"
	if r := notVerifiableReason("linux/amd64", ice); !strings.Contains(r, "toolchain crash/ICE on linux/amd64") {
		t.Errorf("a real ICE must keep the toolchain-crash reason; got %q", r)
	}
}
