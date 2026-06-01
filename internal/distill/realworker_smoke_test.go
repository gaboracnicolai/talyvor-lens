package distill_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/distill"
)

// TestRealWorker_StartsUnderDefaultMemoryLimit spawns the ACTUAL compiled
// distill-worker binary — NOT isolator_test.go's runTestWorker shim, which
// reimplements the protocol inline and bypasses applyMemoryLimit — under the
// PRODUCTION default memory ceiling (DefaultIsolatorMemoryBytes, 512 MiB, which
// the worker turns into an RLIMIT_AS cap on linux), and asserts it starts and
// converts correctly.
//
// WHY THIS EXISTS: the existing isolator tests prove the conversion logic but
// run a shim that never sets RLIMIT_AS, so they cannot tell us whether the real
// worker binary survives STARTUP under the production memory cap. On linux/amd64
// (the prod + CI arch) this is the load-bearing check: if 512 MiB of virtual
// address space is too tight for the Go runtime to start, the real worker dies
// at startup and Convert returns an error → this test fails loudly.
//
// NOTE ON LOCAL RUNS: on darwin, setrlimit(RLIMIT_AS) is rejected and skipped
// (non-fatal), so the cap is never actually applied — a macOS pass is NOT proof.
// The authoritative signal is the linux/amd64 CI run.
func TestRealWorker_StartsUnderDefaultMemoryLimit(t *testing.T) {
	t.Logf("runtime: %s/%s — authoritative assertion is the linux/amd64 CI run", runtime.GOOS, runtime.GOARCH)

	workerBin := buildRealDistillWorker(t)

	// MemoryBytes deliberately UNSET → Convert falls back to
	// DefaultIsolatorMemoryBytes (512 MiB), the exact production default. Do not
	// override it: proving the real binary survives the prod cap is the point.
	iso := &distill.ProcessIsolator{
		WorkerBin: workerBin,
		Timeout:   30 * time.Second,
	}

	res, err := iso.Convert(context.Background(),
		[]byte("<html><body><h1>Hi</h1><p>there</p></body></html>"), distill.FormatHTML)
	if err != nil {
		t.Fatalf("real distill-worker failed under the default %d-byte memory limit "+
			"(RLIMIT_AS too tight for the runtime to start?): %v", distill.DefaultIsolatorMemoryBytes, err)
	}
	if !strings.Contains(res.Markdown, "# Hi") {
		t.Errorf("worker started but produced wrong markdown: %q", res.Markdown)
	}
	if res.NeedsVision {
		t.Errorf("plain HTML must not be flagged NeedsVision; got NeedsVision=true")
	}
}

// buildRealDistillWorker compiles cmd/distill-worker into a temp dir and returns
// the path. Building the actual binary (rather than reusing the test shim) is
// the whole point: only the real main() applies the memory limit at startup.
func buildRealDistillWorker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "distill-worker")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/talyvor/lens/cmd/distill-worker").CombinedOutput()
	if err != nil {
		t.Fatalf("build distill-worker: %v\n%s", err, out)
	}
	return bin
}
