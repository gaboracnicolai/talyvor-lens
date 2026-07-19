package attest

import (
	"os"
	"testing"
)

// THE PRODUCTION BUG THE E2E CAUGHT (PR #325 CI): the extraction dir is bind-mounted read-only at /src and
// the sandbox builds as --user 65534:65534 (buildverify/sandbox.go). os.MkdirTemp creates 0700, so on a real
// Linux daemon the sandbox user cannot traverse the module dir — `go build ./...` fails with "directory
// prefix /src does not contain main module" (no file.go:N: diagnostic) and is misclassified as a toolchain
// crash. This assertion is PERMISSION-BASED (stat the mode), not outcome-based, because Docker Desktop's
// virtiofs masks host ownership/permissions and would hide the bug on macOS: the mode check is meaningful on
// every platform. buildverify's own green fixtures (t.TempDir → 0755) prove 0755 is what the sandbox needs.
func TestExtractionDir_TraversableBySandboxUser(t *testing.T) {
	dir, err := newExtractionDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o055 != 0o055 {
		t.Fatalf("extraction dir mode %04o is not traversable/readable by the sandbox user (--user 65534:65534): "+
			"need o+rx (0755). A 0700 dir makes the sandboxed `go build ./...` fail as \"directory prefix /src "+
			"does not contain main module\" and be misclassified as a toolchain crash — every real Attest on a "+
			"real Linux daemon breaks.", perm)
	}
}
