//go:build windows

package main

// applyMemoryLimit is a no-op on Windows: the parent (ProcessIsolator) assigns
// a Windows Job Object to this process immediately after cmd.Start(), capping
// ProcessMemoryLimit via SetInformationJobObject.  Nothing is needed here in
// the worker because the Job Object constraint is already in effect before any
// significant allocation can occur.
func applyMemoryLimit(_ uint64) {}
