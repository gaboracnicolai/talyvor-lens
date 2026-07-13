package buildverify

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultImage is the pinned toolchain container. It is a specific Go minor line; the EXACT patch version
// that produced a verdict is recorded in Result.Toolchain. PRODUCTION SHOULD pin this by digest
// (golang@sha256:…) so the tag cannot drift under the attestation.
const DefaultImage = "golang:1.25-alpine"

// errResourceKilled marks a build that a resource limit (OOM/pids/etc.) killed — a sandbox failure, NOT a
// compile verdict. Verify maps it to not_verifiable.
var errResourceKilled = errors.New("buildverify: build killed by a resource limit")

// goBuildArgv is the ONLY command Verify ever runs against a source tree. It is `go build` — NEVER `go test`,
// never the target's own binary. `-o /tmp/out/` sends executables to the writable tmpfs (the /src mount is
// read-only); `./...` compiles every package. A compile of a pure-Go (CGO_ENABLED=0) tree executes NO
// attacker code.
var goBuildArgv = []string{"go", "build", "-o", "/tmp/out/", "./..."}

// Verifier is the compile-only, sandboxed build verifier.
type Verifier struct {
	enabled bool
	image   string
	docker  string
	limits  Limits
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithImage overrides the pinned toolchain image. WithDocker overrides the container-runtime binary.
// WithLimits overrides the resource caps.
func WithImage(img string) Option   { return func(v *Verifier) { v.image = img } }
func WithDocker(path string) Option { return func(v *Verifier) { v.docker = path } }
func WithLimits(l Limits) Option    { return func(v *Verifier) { v.limits = l } }

// NewVerifier constructs the verifier. enabled=false (the default posture, gated by LENS_H5_BUILDVERIFY_ENABLED)
// makes Verify return not_verifiable WITHOUT running anything. The container runtime is auto-detected unless
// overridden with WithDocker.
func NewVerifier(enabled bool, opts ...Option) *Verifier {
	v := &Verifier{enabled: enabled, image: DefaultImage, limits: defaultLimits()}
	for _, o := range opts {
		o(v)
	}
	if v.docker == "" {
		if p, err := exec.LookPath("docker"); err == nil {
			v.docker = p
		}
	}
	return v
}

// Verify compiles srcDir in the hard sandbox and returns a compile-only verdict. It NEVER runs tests or the
// target's code. It returns not_verifiable (FAIL OPEN on the verdict) for: disabled; outside the
// deterministic class; or any sandbox/resource failure. It returns not_verifiable + refuses to run when no
// container runtime is present (FAIL CLOSED on containment — there is NO unsandboxed fallback, ever).
func (v *Verifier) Verify(ctx context.Context, srcDir string) Result {
	if !v.enabled {
		return Result{Verdict: NotVerifiable, Reason: "buildverify disabled (LENS_H5_BUILDVERIFY_ENABLED=false)"}
	}
	if !v.dockerAvailable(ctx) {
		// FAIL CLOSED on containment: without a sandbox we refuse — we do NOT run the build unsandboxed.
		return Result{Verdict: NotVerifiable, Reason: "no container runtime available — refusing to compile untrusted code without a sandbox"}
	}
	toolchain := v.toolchainVersion(ctx, srcDir)
	if toolchain == "" {
		return Result{Verdict: NotVerifiable, Reason: "could not determine the sandbox toolchain version"}
	}
	if reason, ok := classify(srcDir, minorOf(toolchain)); !ok {
		return Result{Verdict: NotVerifiable, Reason: reason, Toolchain: toolchain}
	}
	exit, output, infraErr := v.runContained(ctx, srcDir, goBuildArgv)
	if infraErr != nil {
		// timeout / resource kill / docker failure — do NOT emit a verdict.
		return Result{Verdict: NotVerifiable, Reason: "sandbox failure: " + infraErr.Error(), Toolchain: toolchain}
	}
	if exit == 0 {
		return Result{Verdict: Compiled, Toolchain: toolchain}
	}
	// A clean non-zero exit from the toolchain is a real compile failure.
	return Result{Verdict: CompileFailed, Reason: summarize(output), Toolchain: toolchain}
}

// toolchainVersion runs `go version` inside the SAME hardened sandbox and returns e.g. "go1.25.11". The
// source is mounted but ignored by `go version`; running it contained keeps even version discovery isolated.
func (v *Verifier) toolchainVersion(ctx context.Context, srcDir string) string {
	exit, out, err := v.runContained(ctx, srcDir, []string{"go", "version"})
	if err != nil || exit != 0 {
		return ""
	}
	for _, tok := range strings.Fields(out) {
		if strings.HasPrefix(tok, "go1.") {
			return tok
		}
	}
	return ""
}

// minorOf parses N from "go1.N.P".
func minorOf(version string) int {
	v := strings.TrimPrefix(version, "go1.")
	parts := strings.SplitN(v, ".", 2)
	if len(parts) >= 1 {
		if m, err := strconv.Atoi(parts[0]); err == nil {
			return m
		}
	}
	return 0
}

// summarize returns a short, bounded compile-error hint (never a raw dump).
func summarize(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	return "compile failed"
}
