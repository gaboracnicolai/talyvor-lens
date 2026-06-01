//go:build linux || darwin

package main

import (
	"fmt"
	"os"
	"syscall"
)

// applyMemoryLimit sets RLIMIT_AS (virtual address space) on the current
// process to limitBytes.  This is the hard backstop: if GOMEMLIMIT (soft Go
// GC pressure) fails to stop runaway allocation — e.g. a zlib bomb inside
// ledongthuc that inflates through mmap calls the GC cannot see — RLIMIT_AS
// makes the mmap itself fail and the process die with ENOMEM before it can
// consume host memory.
//
// SIZING NOTE: RLIMIT_AS caps VIRTUAL address space, not resident memory.
// The Go runtime on 64-bit systems reserves sparse address space for heap
// arenas; with GOMEMLIMIT already constraining actual heap growth, the
// virtual footprint of a well-behaved process stays well under limitBytes.
// A zlib bomb causes both virtual and resident memory to balloon, so the
// limit catches it.  If a particular host/Go version fails to start with this
// limit, raise DISTILL_WORKER_MEMLIMIT_BYTES in the parent's ProcessIsolator.
func applyMemoryLimit(limitBytes uint64) {
	rl := syscall.Rlimit{Cur: limitBytes, Max: limitBytes}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &rl); err != nil {
		// Non-fatal: GOMEMLIMIT still provides soft protection and the
		// wall-clock timeout is the final kill switch.
		fmt.Fprintf(os.Stderr, "distill-worker: RLIMIT_AS: %v (non-fatal)\n", err)
	}
}
