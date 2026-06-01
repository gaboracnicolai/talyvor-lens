//go:build !linux && !darwin && !windows

package main

// applyMemoryLimit is a no-op on unsupported platforms.
// GOMEMLIMIT (soft) and the wall-clock timeout remain active.
func applyMemoryLimit(_ uint64) {}
