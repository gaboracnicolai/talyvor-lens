//go:build !linux

package coderun

// limitAddressSpace does nothing off Linux: macOS does not enforce RLIMIT_AS (cmd/distill-worker
// memlimit_darwin.go), and Lens runs on Linux.
func limitAddressSpace() {}
