package coderun

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for cmd/lens: started with ChildArg, it is the sandbox child.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ChildArg {
		ChildMain()
	}
	os.Exit(m.Run())
}

func runner(t *testing.T) *Runner {
	t.Helper()
	r, err := NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// B28.119's DONE line: "the 100th prime" worked out in the sandbox prints 541.
func TestRun_TheHundredthPrimeIs541(t *testing.T) {
	got := runner(t).Run(context.Background(), `
const primes = [];
for (let n = 2; primes.length < 100; n++) {
  if (primes.every((p) => n % p !== 0)) primes.push(n);
}
console.log(primes[99]);`)
	if got.Stdout != "541\n" || got.ExitCode != 0 || got.Language != "javascript" {
		t.Fatalf("got %+v; want it to print 541 and exit 0", got)
	}
}

// The child has nothing of Lens's to reach: no environment, no process, no require, no files.
func TestRun_TheCodeReachesNothingOfLens(t *testing.T) {
	t.Setenv("LENS_SECRET_PROBE", "must-not-leak")
	got := runner(t).Run(context.Background(), `[typeof process, typeof require, typeof fetch, typeof Deno, typeof os].join(" ")`)
	if got.Stdout != "undefined undefined undefined undefined undefined\n" {
		t.Fatalf("got %+v; want every host object undefined", got)
	}
	if strings.Contains(got.Stdout+got.Stderr, "must-not-leak") {
		t.Fatal("the child saw Lens's environment")
	}
}

func TestRun_ARunThatNeverEndsIsStopped(t *testing.T) {
	start := time.Now()
	got := runner(t).Run(context.Background(), `while (true) {}`)
	if !got.TimedOut || time.Since(start) > killAfter {
		t.Fatalf("got %+v after %s; want it stopped as timed out within %s", got, time.Since(start), killAfter)
	}
}

func TestRun_ARunThatEatsMemoryIsStopped(t *testing.T) {
	got := runner(t).Run(context.Background(), `const a = []; while (true) a.push(new ArrayBuffer(1 << 26));`)
	if got.ExitCode == 0 || got.ExitCode == 1 || got.TimedOut { // 1 is the code's own error, not a stop
		t.Fatalf("got %+v; want it stopped for memory", got)
	}
	t.Logf("stopped: exit %d, %q", got.ExitCode, got.Stderr)
}
