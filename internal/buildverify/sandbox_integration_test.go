package buildverify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireSandbox returns an enabled Verifier, or SKIPS LOUDLY when no container runtime is present — so a
// missing sandbox reads as "NOT PROVEN HERE", never as a silent pass. (We have been bitten by silent skips:
// the keel schema-isolation gap and the #285 no-op tamper.)
func requireSandbox(t *testing.T, opts ...Option) *Verifier {
	t.Helper()
	v := NewVerifier(true, opts...)
	if !v.dockerAvailable(context.Background()) {
		t.Skipf("⚠ SANDBOX CONTAINMENT NOT VERIFIED HERE — no container runtime (docker) reachable. "+
			"These hostile-containment proofs DID NOT RUN in this environment. Run locally with Docker to verify them. (docker=%q)", v.docker)
	}
	return v
}

func validGoMod(t *testing.T) string {
	return mkmod(t, map[string]string{"go.mod": "module ok\ngo 1.21\n", "main.go": "package main\nfunc main(){ println(\"ok\") }\n"})
}

// CORRECT: a valid pure-Go program compiles, and the verdict names the exact toolchain. Single-platform to
// keep the -race package under the CI 120s cap (multi-platform agreement is covered by its own tests below).
func TestSandbox_ValidGo_Compiled(t *testing.T) {
	v := requireSandbox(t, WithPlatforms("linux/amd64"))
	r := v.Verify(context.Background(), validGoMod(t))
	if r.Verdict != Compiled {
		t.Fatalf("valid pure-Go must compile; got %q (%s)", r.Verdict, r.Reason)
	}
	if !strings.HasPrefix(r.Toolchain, "go1.") {
		t.Errorf("verdict must name the toolchain; got %q", r.Toolchain)
	}
}

// CORRECT + DETERMINISTIC: a type error → compile_failed, byte-identical across N runs (the false-slash
// defense). Single-platform + N=3 to stay under the CI 120s -race cap while still proving determinism.
func TestSandbox_CompileFailed_Deterministic(t *testing.T) {
	v := requireSandbox(t, WithPlatforms("linux/amd64"))
	dir := mkmod(t, map[string]string{"go.mod": "module bad\ngo 1.21\n", "main.go": "package main\nfunc main(){ var x int = \"nope\"; _ = x }\n"})
	first := v.Verify(context.Background(), dir)
	if first.Verdict != CompileFailed {
		t.Fatalf("a type error must be compile_failed; got %q", first.Verdict)
	}
	for i := 0; i < 2; i++ {
		r := v.Verify(context.Background(), dir)
		if r.Verdict != first.Verdict || r.Toolchain != first.Toolchain {
			t.Errorf("run %d differed: %q/%q vs %q/%q — NON-DETERMINISTIC verdict", i+2, r.Verdict, r.Toolchain, first.Verdict, first.Toolchain)
		}
	}
}

// HOSTILE — NETWORK EGRESS: --network=none means a build-time network reach FAILS (and does not hang).
func TestSandbox_NetworkEgress_Blocked(t *testing.T) {
	v := requireSandbox(t)
	// A non-zero/errored run is expected and fine — the point is the build could not REACH the network.
	_, out, _ := v.runContained(context.Background(), validGoMod(t),
		[]string{"sh", "-c", "wget -T 4 -q -O- http://1.1.1.1/ ; echo WGET_EXIT=$?"})
	if strings.Contains(out, "WGET_EXIT=0") {
		t.Errorf("network egress was NOT blocked (wget reached the network): %q", out)
	}
}

// HOSTILE — HOST FILESYSTEM: a host file outside the source mount is NOT readable inside the sandbox.
func TestSandbox_HostFS_Isolated(t *testing.T) {
	v := requireSandbox(t)
	const sentinel = "HOST_SECRET_SENTINEL_9f3a"
	hostFile := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(hostFile, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, _ := v.runContained(context.Background(), validGoMod(t),
		[]string{"sh", "-c", "cat " + hostFile + " 2>&1; cat /etc/host-secret 2>&1; true"})
	if strings.Contains(out, sentinel) {
		t.Errorf("the sandbox READ a host file outside the source mount: %q", out)
	}
}

// HOSTILE — RESOURCE HOG: a CPU-spinning / memory-filling build is KILLED by limits and does NOT hang the host.
func TestSandbox_ResourceHog_Killed(t *testing.T) {
	lim := defaultLimits()
	lim.Timeout = 6 * time.Second
	v := requireSandbox(t, WithLimits(lim))
	start := time.Now()
	_, _, err := v.runContained(context.Background(), validGoMod(t),
		[]string{"sh", "-c", "while :; do :; done"})
	elapsed := time.Since(start)
	if err == nil {
		t.Error("a non-terminating build must be killed (infra error), not succeed")
	}
	if elapsed > lim.Timeout+8*time.Second {
		t.Errorf("resource hog was not killed promptly (%.0fs) — the wall-clock limit failed", elapsed.Seconds())
	}
}

// HOSTILE — ENV SCRUB (live): a host secret set in THIS process is absent from the container's environment.
func TestSandbox_EnvScrubbed_Live(t *testing.T) {
	const secret = "LEAKY_SECRET_VALUE_b7e1"
	t.Setenv("LENS_K4_SECRET", secret)
	t.Setenv("DATABASE_URL", "postgres://"+secret)
	v := requireSandbox(t)
	_, out, _ := v.runContained(context.Background(), validGoMod(t), []string{"env"})
	if strings.Contains(out, secret) {
		t.Errorf("a host secret leaked into the sandbox environment:\n%s", out)
	}
	// sanity: the hermeticity vars ARE present (proves we read the right env).
	if !strings.Contains(out, "GOTOOLCHAIN=local") {
		t.Errorf("expected the hermeticity env in the container; got:\n%s", out)
	}
}

// FIX #2 — ARCH-CONDITIONAL code → not_verifiable. A file named *_arm64.go is built ONLY on arm64 (implicit
// build constraint), so a broken arm64-only file compiles on amd64 but fails on arm64: the platforms
// DISAGREE, so the verdict must be not_verifiable (never a slashable compile_failed on one arch only).
func TestSandbox_ArchConditional_NotVerifiable(t *testing.T) {
	v := requireSandbox(t)
	dir := mkmod(t, map[string]string{
		"go.mod":          "module ac\ngo 1.21\n",
		"main.go":         "package main\nfunc main(){}\n",
		"broken_arm64.go": "package main\nthis is not valid go on arm64\n",
	})
	r := v.Verify(context.Background(), dir)
	if r.Verdict != NotVerifiable {
		t.Errorf("arch-conditional source must be not_verifiable (platforms disagree); got %q (%s)", r.Verdict, r.Reason)
	}
}

// A valid build records the platform SET it agreed across.
func TestSandbox_Compiled_RecordsPlatform(t *testing.T) {
	v := requireSandbox(t)
	r := v.Verify(context.Background(), validGoMod(t))
	if r.Verdict != Compiled {
		t.Fatalf("want compiled; got %q", r.Verdict)
	}
	if !strings.Contains(r.Platform, "linux/amd64") || !strings.Contains(r.Platform, "linux/arm64") {
		t.Errorf("verdict must record the platform agreement set; got %q", r.Platform)
	}
}

// A cgo source is REFUSED (not_verifiable) — never compiled, so no build-time C runs.
func TestSandbox_Cgo_NotVerifiable(t *testing.T) {
	v := requireSandbox(t, WithPlatforms("linux/amd64")) // cgo refuses pre-build; platform irrelevant
	dir := mkmod(t, map[string]string{"go.mod": "module cg\ngo 1.21\n", "main.go": "package main\n\nimport \"C\"\nfunc main(){}\n"})
	if r := v.Verify(context.Background(), dir); r.Verdict != NotVerifiable {
		t.Errorf("cgo must be not_verifiable; got %q", r.Verdict)
	}
}
