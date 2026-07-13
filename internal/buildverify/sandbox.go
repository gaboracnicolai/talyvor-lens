package buildverify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Limits are the per-build resource caps enforced by the container runtime.
type Limits struct {
	Memory   string        // e.g. "512m"
	CPUs     string        // e.g. "1"
	Pids     string        // e.g. "256"
	NoFile   string        // ulimit nofile, e.g. "256"
	Timeout  time.Duration // wall-clock kill
	TmpfsMax string        // /tmp size, e.g. "256m"
}

func defaultLimits() Limits {
	return Limits{Memory: "512m", CPUs: "1", Pids: "256", NoFile: "256", Timeout: 90 * time.Second, TmpfsMax: "256m"}
}

// containerEnv is the COMPLETE, EXPLICIT set of environment variables handed to the build container. It is an
// ALLOW-LIST: the host's environment (os.Environ) is NEVER forwarded, so no LENS_*/DATABASE_URL/API key/cloud
// credential can reach the sandbox. Every entry here is a build-hermeticity control, not a secret.
//
//	CGO_ENABLED=0   no C toolchain, no build-time C execution
//	GOTOOLCHAIN=local  never DOWNLOAD+EXEC another toolchain (a network + code-exec vector)
//	GOFLAGS=-mod=vendor  build only from the vendored tree (offline; also ignores replace directives)
//	GOPROXY=off / GOSUMDB=off  no module fetch
//	GOCACHE/GOPATH/GOMODCACHE/HOME=/tmp  writable scratch on the tmpfs (root FS is read-only)
var containerEnv = []string{
	"CGO_ENABLED=0",
	"GOTOOLCHAIN=local",
	"GOFLAGS=-mod=vendor",
	"GOPROXY=off",
	"GOSUMDB=off",
	"GOCACHE=/tmp/gocache",
	"GOPATH=/tmp/gopath",
	"GOMODCACHE=/tmp/gomodcache",
	"HOME=/tmp",
}

// dockerRunArgs builds the FULL hardened `docker run` argument vector for one contained command. Every
// containment control lives here; there is no code path that runs the command without these.
func dockerRunArgs(srcDir, image string, lim Limits, argv []string) []string {
	args := []string{
		"run", "--rm",
		"--network=none",        // NO network egress
		"--user", "65534:65534", // nobody:nogroup — non-root
		"--security-opt=no-new-privileges",                      // no privilege escalation
		"--cap-drop=ALL",                                        // drop every Linux capability
		"--read-only",                                           // root filesystem read-only
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=" + lim.TmpfsMax, // sole writable area, non-executable
		"--memory", lim.Memory, "--memory-swap", lim.Memory, // RAM cap, no swap
		"--cpus", lim.CPUs,
		"--pids-limit", lim.Pids, // no fork bomb
		"--ulimit", "nofile=" + lim.NoFile + ":" + lim.NoFile,
		"-v", srcDir + ":/src:ro", // source mounted READ-ONLY; no other host mount
		"-w", "/src",
	}
	for _, e := range containerEnv { // explicit allow-list ONLY
		args = append(args, "-e", e)
	}
	args = append(args, image)
	return append(args, argv...)
}

// hostCLIEnv is the minimal env the docker CLI process itself needs on the HOST (to find the binary and reach
// the daemon). It is NOT forwarded into the container. We deliberately do NOT pass os.Environ so the CLI
// process carries no secrets either.
func hostCLIEnv() []string {
	var out []string
	for _, k := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_CONTEXT", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// runContained executes argv inside the hardened container. It returns the inner command's exit code, the
// combined output, and a non-nil err ONLY for a sandbox/infra failure (couldn't start, timed out, or was
// killed by a resource limit) — as opposed to the inner command merely exiting non-zero, which is reported
// via exit. The distinction lets Verify avoid emitting a verdict when the SANDBOX (not the compiler) failed.
func (v *Verifier) runContained(ctx context.Context, srcDir string, argv []string) (exit int, output string, infraErr error) {
	cctx, cancel := context.WithTimeout(ctx, v.limits.Timeout)
	defer cancel()

	// v.docker is a trusted binary path (LookPath/config); the args are STATIC hardened flags + a
	// Talyvor-created temp srcDir path + a hard-coded argv. The UNTRUSTED source tree never enters argv — it
	// is mounted read-only at /src and built INSIDE the container — so no untrusted input can alter the
	// command. Running a container runtime is the entire purpose of this package.
	//nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(cctx, v.docker, dockerRunArgs(srcDir, v.image, v.limits, argv)...)
	cmd.Env = hostCLIEnv() // host CLI env is a minimal allow-list, never os.Environ

	out, err := cmd.CombinedOutput()
	output = string(out)

	if cctx.Err() == context.DeadlineExceeded {
		return -1, output, cctx.Err() // wall-clock kill → sandbox failure, not a verdict
	}
	if err == nil {
		return 0, output, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		// A SIGKILL from the OOM/pids killer surfaces as 137/139/negative — that is a resource kill, NOT a
		// compile verdict. Only a clean non-zero exit from the toolchain (1/2) is a real compile result.
		if code == 137 || code == 139 || code < 0 {
			return code, output, errResourceKilled
		}
		return code, output, nil
	}
	// docker itself failed to run (daemon down, image missing, etc.) → sandbox failure.
	return -1, output, err
}

// dockerAvailable reports whether a usable container runtime is present. FAIL CLOSED: if this is false,
// Verify returns not_verifiable and NEVER falls back to an unsandboxed exec.
func (v *Verifier) dockerAvailable(ctx context.Context) bool {
	if v.docker == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	// `docker version --format {{.Server.Version}}` succeeds only if the daemon is reachable. Fully static
	// argv; v.docker is a trusted path. No untrusted input.
	//nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(cctx, v.docker, "version", "--format", "{{.Server.Version}}")
	cmd.Env = hostCLIEnv()
	out, err := cmd.CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) != ""
}
