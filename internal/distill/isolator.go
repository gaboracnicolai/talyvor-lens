package distill

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// DefaultIsolatorTimeout is the wall-clock budget for one worker conversion.
// 30 s is generous for a 10 MiB PDF on a slow host; tight enough to kill
// runaway decompression before it degrades the host.
const DefaultIsolatorTimeout = 30 * time.Second

// DefaultIsolatorMemoryBytes is the per-worker memory ceiling passed to the
// subprocess.  Sized to sit between legitimate extraction headroom
// (~50–200 MiB from a 10 MiB PDF) and zlib-bomb territory (gigabytes).
// 512 MiB gives a ~50–100× safety margin without false-killing real documents.
const DefaultIsolatorMemoryBytes = 512 << 20 // 512 MiB

// WorkerRequest is the JSON payload the parent sends to the worker on stdin.
// It is also used by the test harness so the test binary can act as a worker.
type WorkerRequest struct {
	Format   string `json:"format"`
	InputB64 string `json:"input_base64"`
}

// WorkerResponse is the JSON payload the worker writes to stdout.
type WorkerResponse struct {
	Markdown    string   `json:"markdown"`
	Format      string   `json:"format"`
	NeedsVision bool     `json:"needs_vision"`
	Warnings    []string `json:"warnings,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// ProcessIsolator runs a single document conversion inside a disposable
// subprocess.  The worker lifetime is bounded by a wall-clock timeout
// (exec.CommandContext → SIGKILL / TerminateProcess, fully cross-platform)
// and a memory ceiling (GOMEMLIMIT soft + RLIMIT_AS on Unix /
// Windows Job Object hard).
//
// This addresses the residual risk documented in pdf.go: Go's recover() cannot
// catch OOM (fatal signal) or an infinite loop inside ledongthuc/pdf (a runaway
// goroutine cannot be killed from outside), but SIGKILL / TerminateProcess can
// always stop the subprocess regardless of what it is doing.
type ProcessIsolator struct {
	// WorkerBin is the path to the compiled distill-worker executable.
	// Required — no default; the integration point (e.g. cmd/lens) must supply
	// the deployment path.
	WorkerBin string
	// Timeout is the wall-clock kill deadline.  Zero uses DefaultIsolatorTimeout.
	Timeout time.Duration
	// MemoryBytes is the per-worker memory ceiling in bytes.
	// Zero uses DefaultIsolatorMemoryBytes.
	MemoryBytes uint64
	// ExtraEnv holds additional environment variables appended to the worker's
	// inherited environment.  Primarily used in tests to inject
	// DISTILL_WORKER_SUBPROCESS=1 so the test binary can act as the worker.
	ExtraEnv []string
}

// Convert runs input through the worker subprocess as the given format and
// returns the converted Result.  The subprocess is killed unconditionally if
// it exceeds Timeout.
func (p *ProcessIsolator) Convert(ctx context.Context, input []byte, format Format) (Result, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = DefaultIsolatorTimeout
	}
	memBytes := p.MemoryBytes
	if memBytes == 0 {
		memBytes = DefaultIsolatorMemoryBytes
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the request payload.
	req := WorkerRequest{
		Format:   string(format),
		InputB64: base64.StdEncoding.EncodeToString(input),
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return Result{Format: format}, fmt.Errorf("distill: isolator marshal: %w", err)
	}

	cmd := exec.CommandContext(tctx, p.WorkerBin)

	// Inherit the parent's environment, then:
	//   GOMEMLIMIT — soft ceiling: Go GC applies pressure before actual usage
	//               reaches memBytes, reducing the chance of a hard kill on
	//               legitimate documents.
	//   DISTILL_WORKER_MEMLIMIT_BYTES — read by applyMemoryLimit() in the worker
	//               binary (memlimit_*.go) to set the platform hard ceiling.
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("GOMEMLIMIT=%d", memBytes),
		fmt.Sprintf("DISTILL_WORKER_MEMLIMIT_BYTES=%d", memBytes),
	)
	if len(p.ExtraEnv) > 0 {
		cmd.Env = append(cmd.Env, p.ExtraEnv...)
	}

	// Stderr flows to the parent for observability; stdout carries the JSON
	// protocol; stdin delivers the request.
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{Format: format}, fmt.Errorf("distill: isolator stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{Format: format}, fmt.Errorf("distill: isolator stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Result{Format: format}, fmt.Errorf("distill: isolator start: %w", err)
	}

	// Apply the platform-specific hard memory ceiling on the live process.
	// Non-fatal if it fails — GOMEMLIMIT is still active and the wall-clock
	// timeout is the final kill switch.  applyJobLimit is defined in
	// isolator_posthook_*.go.
	if err := applyJobLimit(cmd, memBytes); err != nil {
		slog.Warn("distill: isolator memory ceiling unavailable",
			slog.String("err", err.Error()),
			slog.String("fallback", "GOMEMLIMIT soft limit still active"),
		)
	}

	// Deliver the request, then close stdin to signal EOF to the worker.
	if _, err := stdin.Write(reqBytes); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // collect exit status — prevents a zombie on Unix / leaked handle on Windows
		return Result{Format: format}, fmt.Errorf("distill: isolator write stdin: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait() // same: must reap the process even on the error path
		return Result{Format: format}, fmt.Errorf("distill: isolator close stdin: %w", err)
	}

	// Read the response before Wait; the JSON decoder returns on EOF (worker
	// closed stdout) even before Wait returns.
	var resp WorkerResponse
	decodeErr := json.NewDecoder(stdout).Decode(&resp)

	waitErr := cmd.Wait()

	// Decode failure: check whether the context deadline fired first (the most
	// informative error to surface to the caller).
	if decodeErr != nil {
		if tctx.Err() != nil {
			return Result{Format: format}, fmt.Errorf("%w: worker timed out after %s",
				ErrConversionFailed, timeout)
		}
		return Result{Format: format}, fmt.Errorf("%w: decode worker response: %v",
			ErrConversionFailed, decodeErr)
	}

	// Non-zero exit but we have a decoded response — prefer the structured
	// error from the worker over the generic exit-code message.
	if waitErr != nil && resp.Error == "" {
		if tctx.Err() != nil {
			return Result{Format: format}, fmt.Errorf("%w: worker timed out after %s",
				ErrConversionFailed, timeout)
		}
		return Result{Format: format}, fmt.Errorf("%w: worker exited: %v",
			ErrConversionFailed, waitErr)
	}

	if resp.Error != "" {
		return Result{Format: Format(resp.Format)}, fmt.Errorf("%w: %s",
			ErrConversionFailed, resp.Error)
	}

	return Result{
		Markdown:    resp.Markdown,
		Format:      Format(resp.Format),
		NeedsVision: resp.NeedsVision,
		Warnings:    resp.Warnings,
	}, nil
}
