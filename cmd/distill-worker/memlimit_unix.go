//go:build linux

package main

import (
	"fmt"
	"math"
	"os"
	"syscall"
)

// virtualAddressHeadroom is added to the resident ceiling when sizing RLIMIT_AS.
//
// RLIMIT_AS caps VIRTUAL address space, not resident memory, and the Go runtime
// reserves far more virtual space than it ever makes resident: heap-arena
// reservations, per-P mcaches, goroutine stacks, and mmap'd runtime metadata all
// count against RLIMIT_AS while contributing little to RSS. Go 1.26 enlarged
// that baseline virtual footprint past the old 512 MiB ceiling, so a worker
// whose RLIMIT_AS equalled its resident ceiling died with a fatal
// "runtime: out of memory" while mapping arenas — even for a trivial document.
//
// 2 GiB of headroom comfortably covers the runtime's reservations (and leaves
// room for future toolchain growth) while keeping the backstop bounded: a true
// runaway is still independently constrained by GOMEMLIMIT (soft GC pressure on
// the Go heap) and the parent's wall-clock SIGKILL, both of which act on
// resident memory regardless of this virtual ceiling.
const virtualAddressHeadroom = 2 << 30 // 2 GiB

// applyMemoryLimit sets RLIMIT_AS (virtual address space) on the current
// process. limitBytes is the RESIDENT ceiling (the same value the parent passes
// via GOMEMLIMIT); RLIMIT_AS is sized at limitBytes + virtualAddressHeadroom so
// the Go runtime's virtual reservations do not trip the cap at startup.
//
// This is the hard backstop: if GOMEMLIMIT (soft Go GC pressure) fails to stop
// runaway allocation — e.g. a zlib bomb inside ledongthuc that inflates through
// mmap calls the GC cannot see — RLIMIT_AS makes the mmap itself fail and the
// process die with ENOMEM before it can exhaust host memory.
func applyMemoryLimit(limitBytes uint64) {
	asLimit := uint64(math.MaxUint64) // saturate rather than wrap on overflow
	if limitBytes <= math.MaxUint64-virtualAddressHeadroom {
		asLimit = limitBytes + virtualAddressHeadroom
	}
	rl := syscall.Rlimit{Cur: asLimit, Max: asLimit}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &rl); err != nil {
		// Non-fatal: GOMEMLIMIT still provides soft protection and the
		// wall-clock timeout is the final kill switch.
		fmt.Fprintf(os.Stderr, "distill-worker: RLIMIT_AS: %v (non-fatal)\n", err)
	}
}
