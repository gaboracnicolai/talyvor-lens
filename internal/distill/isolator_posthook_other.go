//go:build !linux && !darwin && !windows

package distill

import "os/exec"

// applyJobLimit is a no-op on unsupported platforms.
// GOMEMLIMIT (soft) and the wall-clock timeout remain active.
func applyJobLimit(_ *exec.Cmd, _ uint64) error { return nil }
