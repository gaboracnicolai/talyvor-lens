//go:build darwin

package main

// applyMemoryLimit is intentionally a no-op on macOS.
//
// RLIMIT_AS exists in the Darwin syscall table and syscall.Setrlimit returns
// without error, but the macOS kernel does not actually enforce the limit —
// a process can exceed the configured RLIMIT_AS ceiling with no consequence.
// Setting it would give false confidence while providing zero protection.
//
// Memory protection on macOS falls back to:
//   - GOMEMLIMIT — Go runtime GC pressure that fires before real allocation
//     reaches the ceiling (set by the parent via the GOMEMLIMIT env var).
//   - Wall-clock timeout — exec.CommandContext sends SIGKILL when the
//     deadline fires, which cannot be caught or blocked regardless of how
//     much memory the subprocess has consumed.
func applyMemoryLimit(_ uint64) {}
