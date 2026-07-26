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
