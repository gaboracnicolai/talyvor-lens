// Command distill-worker is a disposable subprocess that converts a single
// document and exits. The parent (ProcessIsolator) communicates over
// stdin/stdout using a JSON request/response protocol and kills the process
// unconditionally if it exceeds the wall-clock deadline.
//
// Running conversion in a separate process is the "stage-3" hardening called
// for in pdf.go: Go's recover() cannot catch OOM (fatal) or cyclic-ref hangs
// (goroutine cannot be killed externally). The worker can be SIGKILL'd / had
// TerminateProcess called on it no matter what ledongthuc/pdf is doing.
//
// Memory ceiling: the parent sets GOMEMLIMIT (soft Go GC limit) and
// DISTILL_WORKER_MEMLIMIT_BYTES for the platform-specific hard ceiling.
// applyMemoryLimit() (memlimit_*.go) reads the latter and enforces it via
// RLIMIT_AS on Unix or is a no-op on Windows (where the parent sets a
// Job Object after cmd.Start).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/talyvor/lens/internal/distill"
)

// request mirrors distill.WorkerRequest — duplicated here to keep the worker
// binary self-contained and to avoid the import cycle that would arise if
// isolator.go imported cmd/distill-worker.
type request struct {
	Format   string `json:"format"`
	InputB64 string `json:"input_base64"`
}

// response mirrors distill.WorkerResponse.
type response struct {
	Markdown    string   `json:"markdown"`
	Format      string   `json:"format"`
	NeedsVision bool     `json:"needs_vision"`
	Warnings    []string `json:"warnings,omitempty"`
	Error       string   `json:"error,omitempty"`
}

func main() {
	// Apply the platform-specific hard memory ceiling BEFORE reading any
	// untrusted input.  applyMemoryLimit is defined in memlimit_*.go.
	applyMemoryLimit(memLimitBytes())

	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		writeResponse(response{Error: fmt.Sprintf("distill-worker: decode request: %v", err)})
		os.Exit(1)
	}

	input, err := base64.StdEncoding.DecodeString(req.InputB64)
	if err != nil {
		writeResponse(response{Error: fmt.Sprintf("distill-worker: decode base64: %v", err)})
		os.Exit(1)
	}

	res, convErr := distill.DistillAs(context.Background(), input, distill.Format(req.Format))

	resp := response{
		Markdown:    res.Markdown,
		Format:      string(res.Format),
		NeedsVision: res.NeedsVision,
		Warnings:    res.Warnings,
	}
	if convErr != nil {
		resp.Error = convErr.Error()
	}
	writeResponse(resp)
}

// writeResponse encodes resp as JSON to stdout.  An encoding failure is
// unrecoverable — the parent will read EOF and report a decode error.
func writeResponse(resp response) {
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		fmt.Fprintf(os.Stderr, "distill-worker: encode response: %v\n", err)
		os.Exit(1)
	}
}

// memLimitBytes reads the memory ceiling from DISTILL_WORKER_MEMLIMIT_BYTES
// (set by the parent via ProcessIsolator).  Falls back to 512 MiB.
func memLimitBytes() uint64 {
	if s := os.Getenv("DISTILL_WORKER_MEMLIMIT_BYTES"); s != "" {
		if v, err := strconv.ParseUint(s, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return 512 << 20 // 512 MiB default
}
