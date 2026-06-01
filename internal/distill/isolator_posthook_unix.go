//go:build linux || darwin

package distill

import "os/exec"

// applyJobLimit is a no-op on Unix: the worker process sets RLIMIT_AS on
// itself at startup via memlimit_unix.go, so no parent-side action is needed.
// GOMEMLIMIT (set via env var in ProcessIsolator.Convert) is the soft ceiling.
func applyJobLimit(_ *exec.Cmd, _ uint64) error { return nil }
