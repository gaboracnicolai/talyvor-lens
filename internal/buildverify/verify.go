package buildverify

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
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

// defaultPlatforms is the set every verdict must AGREE across. compile_failed is emitted ONLY when the tree
// fails on ALL of them (a platform-independent failure); if results differ (arch-conditional via //go:build),
// the verdict is not_verifiable — closing the cross-arch false-slash hole. Pure-Go cross-compile (CGO=0) is cheap.
var defaultPlatforms = []string{"linux/amd64", "linux/arm64"}

// Verifier is the compile-only, sandboxed build verifier.
type Verifier struct {
	enabled   bool
	image     string
	docker    string
	limits    Limits
	platforms []string
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithImage overrides the pinned toolchain image. WithDocker overrides the container-runtime binary.
// WithLimits overrides the resource caps. WithPlatforms overrides the GOOS/GOARCH agreement set.
func WithImage(img string) Option      { return func(v *Verifier) { v.image = img } }
func WithDocker(path string) Option    { return func(v *Verifier) { v.docker = path } }
func WithLimits(l Limits) Option       { return func(v *Verifier) { v.limits = l } }
func WithPlatforms(p ...string) Option { return func(v *Verifier) { v.platforms = p } }

// NewVerifier constructs the verifier. enabled=false (the default posture, gated by LENS_H5_BUILDVERIFY_ENABLED)
// makes Verify return not_verifiable WITHOUT running anything. The container runtime is auto-detected unless
// overridden with WithDocker.
func NewVerifier(enabled bool, opts ...Option) *Verifier {
	v := &Verifier{enabled: enabled, image: DefaultImage, limits: defaultLimits(), platforms: defaultPlatforms}
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
	platformSet := strings.Join(v.platforms, ",")
	// Build on EVERY target platform and require agreement. Any not_verifiable, or a disagreement between
	// platforms (arch-conditional //go:build), yields not_verifiable — never a slashable verdict.
	var agreed Verdict
	var reason string
	for i, plat := range v.platforms {
		goos, goarch, ok := splitPlatform(plat)
		if !ok {
			return Result{Verdict: NotVerifiable, Reason: "bad platform spec " + plat, Toolchain: toolchain, Platform: platformSet}
		}
		exit, output, infraErr := v.runContained(ctx, srcDir, goBuildArgv, "GOOS="+goos, "GOARCH="+goarch)
		if infraErr != nil {
			return Result{Verdict: NotVerifiable, Reason: "sandbox failure: " + infraErr.Error(), Toolchain: toolchain, Platform: platformSet}
		}
		vd := classifyBuildResult(exit, output)
		if vd == NotVerifiable {
			return Result{Verdict: NotVerifiable, Reason: notVerifiableReason(plat, output), Toolchain: toolchain, Platform: platformSet}
		}
		if i == 0 {
			agreed, reason = vd, summarize(output)
		} else if vd != agreed {
			// e.g. compiles on amd64 but fails on arm64 → arch-conditional → refuse.
			return Result{Verdict: NotVerifiable, Reason: "platforms disagree (arch-conditional build) — refusing", Toolchain: toolchain, Platform: platformSet}
		}
	}
	if agreed == CompileFailed {
		return Result{Verdict: CompileFailed, Reason: reason, Toolchain: toolchain, Platform: platformSet}
	}
	return Result{Verdict: Compiled, Toolchain: toolchain, Platform: platformSet}
}

// classifyBuildResult maps ONE build's (exit, output) to a verdict. It emits compile_failed ONLY for a CLEAN
// diagnostic-style failure (a `file.go:line:col:` message with exit 1/2). A toolchain crash / internal
// compiler error / panic / signal — or any non-diagnostic non-zero exit — is NOT a trustworthy compile
// verdict and maps to NotVerifiable. Err toward refusing: a wrong compile_failed becomes a FALSE SLASH.
func classifyBuildResult(exit int, output string) Verdict {
	if exit == 0 {
		return Compiled
	}
	low := strings.ToLower(output)
	for _, marker := range []string{
		"internal compiler error", "compiler bug", "signal sigsegv", "signal: segmentation",
		"panic:", "fatal error:", "goroutine ", "runtime:", "out of memory", "killed",
	} {
		if strings.Contains(low, marker) {
			return NotVerifiable
		}
	}
	if (exit == 1 || exit == 2) && diagnosticLine.MatchString(output) {
		return CompileFailed
	}
	return NotVerifiable
}

// diagnosticLine matches a Go compiler diagnostic (path.go:line[:col]:).
var diagnosticLine = regexp.MustCompile(`\.go:\d+(:\d+)?:`)

// notVerifiableReason names a non-verdict build failure. A failure where go could not even SEE the module —
// "does not contain main module" / "go.mod file not found" / a permission denial — is an INVOCATION problem
// (srcDir permissions/ownership vs the sandbox's non-root user), not a compiler crash; labeling it
// "toolchain crash/ICE" made exactly that bug (a 0700 extraction dir) indistinguishable from a real ICE.
// The verdict is NotVerifiable either way (fail-open unchanged); only the reason is sharpened.
func notVerifiableReason(plat, output string) string {
	low := strings.ToLower(output)
	for _, marker := range []string{"does not contain main module", "go.mod file not found", "permission denied"} {
		if strings.Contains(low, marker) {
			return "module unreadable or missing in the sandbox on " + plat +
				" (srcDir permissions/ownership vs the sandbox user?) — refusing: " + summarize(output)
		}
	}
	return "toolchain crash/ICE on " + plat + " — refusing"
}

// splitPlatform splits "linux/amd64" into ("linux","amd64",true).
func splitPlatform(p string) (goos, goarch string, ok bool) {
	parts := strings.SplitN(p, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
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
