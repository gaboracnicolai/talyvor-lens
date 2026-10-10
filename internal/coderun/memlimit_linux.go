//go:build linux

package coderun

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// limitAddressSpace caps the child's virtual memory at what it already maps plus 1 GiB, so an allocation too big
// and too quick for Eval's heap check fails in the child instead of growing it. It is sized from the child's own
// size, not a constant: the Go runtime's reservations (and the race detector's shadow, under -race) are already
// mapped by now, and a fixed cap below them would fail the first allocation (cmd/distill-worker memlimit_unix.go).
func limitAddressSpace() {
	statm, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return
	}
	f := strings.Fields(string(statm))
	if len(f) == 0 {
		return
	}
	pages, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return
	}
	lim := pages*uint64(os.Getpagesize()) + 1<<30
	_ = syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: lim, Max: lim})
}
