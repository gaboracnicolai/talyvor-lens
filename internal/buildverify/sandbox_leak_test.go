package buildverify

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sandbox_leak_test.go — a timed-out sandbox run must leave NO container behind.
//
// ⚠ A PRODUCTION BUG, NOT A TEST BUG. runContained enforces its wall clock with
// exec.CommandContext, which SIGKILLs the `docker run` CLI on deadline. But the container is a
// child of the DAEMON, not of the CLI, and `--rm` is implemented BY the CLI (it waits for exit,
// then removes). Kill the client and the container survives with nothing left to reap it.
//
// Confirmed on a live orphan before the fix — `docker inspect` on a container found running on
// this machine reported:
//
//	AutoRemove: true                              ← --rm WAS set
//	Cmd:        ["sh","-c","while :; do :; done"]
//	Mounts:     …/TestSandbox_ResourceHog_Killed3651098285/001 -> /src
//	StartedAt:  3.7 hours earlier, still at 100.91% CPU
//
// So `--rm` was on and the container was still there: exactly the claimed mechanism, and the
// reason `--rm` alone cannot be the answer. In production this leaks a full core (--cpus 1) per
// timed-out build, permanently and silently. The test only made it visible.
//
// ⚠ THE ASSERTION IS ABSENCE FROM `docker ps`, not that runContained returned. Returning is what
// it already did while leaking.

// runningContainerNames returns the names of currently-running containers. Uses the docker CLI
// directly so the assertion observes the DAEMON's view, not the sandbox's own bookkeeping — a fix
// that merely recorded "I reaped it" would pass a self-reported check and still leak.
func runningContainerNames(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return string(out)
}

// reapByPrefix force-removes anything left matching a prefix. Registered with t.Cleanup by the
// leak test itself so a FAILING run — i.e. the leak still present — does not leave a CPU hog on
// the machine.
//
// ⚠ A TEST PROVING A KILL MUST NOT DEPEND ON THE FIX IT TESTS. If the sandbox's reaper is broken,
// this is what stops the test from becoming the very leak it is asserting against.
func reapByPrefix(t *testing.T, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name="+prefix).Output()
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = exec.CommandContext(rmCtx, "docker", "rm", "-f", id).Run()
		rmCancel()
		t.Logf("cleanup: force-removed leftover sandbox container %s", id)
	}
}

// TestSandbox_TimedOutRun_LeavesNoContainer is the leak assertion.
//
// The workload is a CPU spinner because that is the shape that leaks — a process that ignores the
// client going away. It is bounded three ways so this test cannot itself become the problem:
// the sandbox's own short Timeout, the container's --cpus/--memory limits, and the t.Cleanup reap
// above which runs on pass, fail and panic alike.
func TestSandbox_TimedOutRun_LeavesNoContainer(t *testing.T) {
	lim := defaultLimits()
	lim.Timeout = 4 * time.Second // short: this test spins a core for exactly this long
	v := requireSandbox(t, WithLimits(lim))

	// Registered BEFORE the run, so an early failure still reaps.
	t.Cleanup(func() { reapByPrefix(t, sandboxContainerPrefix) })

	before := runningContainerNames(t)

	start := time.Now()
	_, _, err := v.runContained(context.Background(), validGoMod(t),
		[]string{"sh", "-c", "while :; do :; done"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a non-terminating build must be killed (infra error), not succeed")
	}
	if elapsed > lim.Timeout+20*time.Second {
		t.Fatalf("not killed promptly (%.0fs)", elapsed.Seconds())
	}

	// THE ASSERTION. Give the daemon a moment to settle, then ask IT what is running.
	deadline := time.Now().Add(20 * time.Second)
	var leaked []string
	for time.Now().Before(deadline) {
		leaked = nil
		for _, name := range strings.Fields(runningContainerNames(t)) {
			if strings.HasPrefix(name, sandboxContainerPrefix) {
				leaked = append(leaked, name)
			}
		}
		if len(leaked) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(leaked) > 0 {
		t.Errorf("a timed-out sandbox run left %d container(s) RUNNING: %v\n"+
			"`--rm` is client-side and the timeout kills the client, so the container outlives it "+
			"with nothing to reap it — each one pegs a full core (--cpus %s) forever.\n"+
			"before: %s", len(leaked), leaked, lim.CPUs, strings.TrimSpace(before))
	}
}

// TestSandbox_HappyPath_LeavesNoContainer — the reaper must not depend on the timeout path, and a
// normal run must not leave anything either (nor should the explicit reap break --rm's own work).
func TestSandbox_HappyPath_LeavesNoContainer(t *testing.T) {
	v := requireSandbox(t)
	t.Cleanup(func() { reapByPrefix(t, sandboxContainerPrefix) })

	if _, _, err := v.runContained(context.Background(), validGoMod(t),
		[]string{"sh", "-c", "true"}); err != nil {
		t.Fatalf("a trivial contained command must succeed: %v", err)
	}

	for _, name := range strings.Fields(runningContainerNames(t)) {
		if strings.HasPrefix(name, sandboxContainerPrefix) {
			t.Errorf("a SUCCESSFUL run left container %q running", name)
		}
	}
}

// TestSandboxContainerName_IsUniqueAndGreppable — the reaper needs a name it can target, and two
// concurrent verifications must not collide on it (a collision would make `docker run` fail with
// "name already in use", turning a leak into an outage).
func TestSandboxContainerName_IsUniqueAndGreppable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		n := newSandboxContainerName()
		if !strings.HasPrefix(n, sandboxContainerPrefix) {
			t.Fatalf("name %q lacks the prefix the reaper filters on", n)
		}
		if seen[n] {
			t.Fatalf("duplicate container name %q after %d draws — a collision makes docker run "+
				"fail outright", n, i)
		}
		seen[n] = true
	}
}

// TestDockerRunArgs_NamesTheContainerAndKeepsRm — `--rm` STAYS. It is not sufficient (it dies with
// the client) but it is still the thing that cleans up every non-timeout path promptly, and
// belt-and-braces is the point.
func TestDockerRunArgs_NamesTheContainerAndKeepsRm(t *testing.T) {
	args := dockerRunArgs("/tmp/src", "golang:1.25-alpine", "name-me", defaultLimits(), []string{"true"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--rm") {
		t.Errorf("--rm was removed; it must stay as belt-and-braces: %s", joined)
	}
	if !strings.Contains(joined, "--name name-me") {
		t.Errorf("the container is not named, so nothing can reap it by name: %s", joined)
	}
}
