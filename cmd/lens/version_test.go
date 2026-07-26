package main

// The version is reported by four public surfaces — /healthz, /status, the
// service page at /, and the MCP server — and was, until now, five independent
// bare literals plus one named constant. Nothing linked them, so bumping the
// release would have left surfaces disagreeing about what was deployed, and the
// disagreement would have been invisible until a customer quoted the wrong
// number on a ticket.
//
// This is the same defect class as a money constant copied out of Go with no
// link to its source: correct on the day it is written, silently wrong later.
// It is caught the same way — by a test that fails when a second copy appears.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// versionLiteral matches a quoted dotted-numeric version, e.g. "0.1.0".
var versionLiteral = regexp.MustCompile(`"\d+\.\d+\.\d+"`)

func TestVersionHasASingleSource(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			// Prose may quote a version while discussing this very rule — the
			// declaration's own doc comment does. Only code counts. (Caught by
			// positive-controlling this test: without it, the guard was red on a
			// correct tree, which is a gate nobody could ever get green.)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !versionLiteral.MatchString(line) {
				continue
			}
			// The one permitted occurrence: the declaration itself.
			if strings.Contains(line, "const lensVersion") {
				continue
			}
			offenders = append(offenders, filepath.Base(f)+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("version literal duplicated outside the lensVersion declaration — "+
			"every surface must read the one constant, or deployments will disagree "+
			"about what shipped:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestVersionDefaultIsAnHonestPlaceholder guards the OTHER half of this class.
//
// TestVersionHasASingleSource stops the version being copied. This stops the
// single source from lying: a default of "0.1.0" looks like a version while
// nothing sets it, which is how every deployment came to report 0.1.0 no matter
// which commit shipped — and why deployed SHAs had to be verified by hand
// against the registry.
//
// The default must therefore be visibly NOT a version. The complementary check
// — that a release binary carries a real stamp — cannot be made from a unit
// test, because nothing here can see whether the link step ran; CI asserts it
// against the built artifact instead (see the build job).
func TestVersionDefaultIsAnHonestPlaceholder(t *testing.T) {
	if versionLiteral.MatchString(`"` + lensVersion + `"`) {
		t.Errorf("lensVersion defaults to %q, which is version-SHAPED — an unstamped build "+
			"would claim a version it does not have. Use an obviously-unset placeholder.", lensVersion)
	}
	for _, ok := range []string{"dev", "unknown", "none"} {
		if lensVersion == ok {
			return
		}
	}
	t.Errorf("lensVersion defaults to %q; expected an explicit placeholder such as \"dev\"", lensVersion)
}
