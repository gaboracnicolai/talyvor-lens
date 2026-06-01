package distill

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ── Subprocess worker harness ────────────────────────────────────────────────
//
// TestMain makes this test binary double as a distill-worker subprocess.
// When DISTILL_WORKER_SUBPROCESS=1 is set the binary acts as a worker:
// it reads a WorkerRequest JSON from stdin, calls DistillAs, and writes a
// WorkerResponse JSON to stdout — then exits.  Tests point ProcessIsolator at
// os.Args[0] and inject DISTILL_WORKER_SUBPROCESS=1 via ExtraEnv, so every
// subprocess test exercises the real isolator code-path without needing a
// pre-built distill-worker binary.
//
// When DISTILL_WORKER_HANG=1 is ALSO set the worker sleeps for ten minutes to
// simulate a hung ledongthuc/pdf parse; the timeout test uses this to verify
// that the isolator kills the subprocess within its deadline.

func TestMain(m *testing.M) {
	switch os.Getenv("DISTILL_WORKER_SUBPROCESS") {
	case "1":
		runTestWorker()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runTestWorker implements the distill-worker JSON protocol inline so that the
// test binary can serve as the subprocess target.
func runTestWorker() {
	if os.Getenv("DISTILL_WORKER_HANG") == "1" {
		// Simulate a hung PDF parse (cyclic object graph / zlib decompression
		// with no progress).  The isolator must kill us via its deadline.
		time.Sleep(10 * time.Minute)
		return
	}

	var req WorkerRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		writeTestWorkerErr(fmt.Sprintf("decode request: %v", err))
		return
	}

	input, err := base64.StdEncoding.DecodeString(req.InputB64)
	if err != nil {
		writeTestWorkerErr(fmt.Sprintf("decode base64: %v", err))
		return
	}

	res, convErr := DistillAs(context.Background(), input, Format(req.Format))
	resp := WorkerResponse{
		Markdown:    res.Markdown,
		Format:      string(res.Format),
		NeedsVision: res.NeedsVision,
		Warnings:    res.Warnings,
	}
	if convErr != nil {
		resp.Error = convErr.Error()
	}
	json.NewEncoder(os.Stdout).Encode(resp) //nolint:errcheck
}

func writeTestWorkerErr(msg string) {
	json.NewEncoder(os.Stdout).Encode(WorkerResponse{Error: msg}) //nolint:errcheck
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// testIsolator returns a ProcessIsolator that points at this test binary.
func testIsolator(t *testing.T, timeout time.Duration) *ProcessIsolator {
	t.Helper()
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &ProcessIsolator{
		WorkerBin: os.Args[0],
		Timeout:   timeout,
		ExtraEnv:  []string{"DISTILL_WORKER_SUBPROCESS=1"},
	}
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestIsolator_PlainText verifies the sunny-path: a plain text document is
// converted through the subprocess and returned to the caller intact.
func TestIsolator_PlainText(t *testing.T) {
	iso := testIsolator(t, 0)
	res, err := iso.Convert(context.Background(), []byte("Hello, isolated world!"), FormatText)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if !strings.Contains(res.Markdown, "Hello, isolated world!") {
		t.Errorf("expected text in markdown; got %q", res.Markdown)
	}
	if res.Format != FormatText {
		t.Errorf("Format = %q, want %q", res.Format, FormatText)
	}
}

// TestIsolator_PDF_TextLayer verifies that a PDF with a real text layer is
// extracted correctly through the subprocess.
func TestIsolator_PDF_TextLayer(t *testing.T) {
	iso := testIsolator(t, 0)
	pdf := buildPDF("Isolated PDF extraction", "line two")
	res, err := iso.Convert(context.Background(), pdf, FormatPDF)
	if err != nil {
		t.Fatalf("Convert PDF: %v", err)
	}
	if res.NeedsVision {
		t.Errorf("PDF with text layer must not set NeedsVision")
	}
	if !strings.Contains(res.Markdown, "Isolated PDF extraction") {
		t.Errorf("expected extracted text in markdown; got %q", res.Markdown)
	}
}

// TestIsolator_PDF_TextLess verifies that a scan/image-only PDF correctly
// propagates NeedsVision=true through the subprocess.
func TestIsolator_PDF_TextLess(t *testing.T) {
	iso := testIsolator(t, 0)
	pdf := buildPDF() // no body → no text operators
	res, err := iso.Convert(context.Background(), pdf, FormatPDF)
	if err != nil {
		t.Fatalf("Convert text-less PDF: %v", err)
	}
	if !res.NeedsVision {
		t.Errorf("text-less PDF must set NeedsVision=true; got md=%q", res.Markdown)
	}
}

// TestIsolator_ErrorPropagation verifies that conversion errors in the worker
// (e.g. unsupported format) are returned as errors to the caller — not silently
// swallowed or returned as empty Markdown.
func TestIsolator_ErrorPropagation(t *testing.T) {
	iso := testIsolator(t, 0)
	// FormatUnknown has no registered converter → ErrUnsupportedFormat.
	_, err := iso.Convert(context.Background(), []byte("data"), FormatUnknown)
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
}

// TestIsolator_EmptyInput verifies that the ErrEmptyInput sentinel is
// propagated correctly through the subprocess protocol.
func TestIsolator_EmptyInput(t *testing.T) {
	iso := testIsolator(t, 0)
	_, err := iso.Convert(context.Background(), []byte{}, FormatText)
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

// TestIsolator_Timeout verifies that a hung worker is killed within the
// configured deadline and that the caller receives a meaningful error.
// DISTILL_WORKER_HANG=1 makes the test-binary worker sleep for 10 minutes;
// the isolator must kill it and return within the 300 ms deadline.
func TestIsolator_Timeout(t *testing.T) {
	iso := &ProcessIsolator{
		WorkerBin: os.Args[0],
		Timeout:   300 * time.Millisecond,
		ExtraEnv: []string{
			"DISTILL_WORKER_SUBPROCESS=1",
			"DISTILL_WORKER_HANG=1",
		},
	}
	start := time.Now()
	_, err := iso.Convert(context.Background(), []byte("x"), FormatText)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error; got: %v", err)
	}
	// The kill+wait round-trip should complete well within 2 s even on a
	// loaded CI host.
	if elapsed > 2*time.Second {
		t.Errorf("isolator took %s to kill worker; expected <2s", elapsed)
	}
}

// TestIsolator_ContextCancellation verifies that cancelling the caller's
// context propagates to the subprocess (via the merged context timeout).
func TestIsolator_ContextCancellation(t *testing.T) {
	iso := &ProcessIsolator{
		WorkerBin: os.Args[0],
		Timeout:   30 * time.Second, // isolator timeout is generous
		ExtraEnv: []string{
			"DISTILL_WORKER_SUBPROCESS=1",
			"DISTILL_WORKER_HANG=1",
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err := iso.Convert(ctx, []byte("x"), FormatText)
	if err == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
}

// TestIsolator_WorkerBinMissing verifies that a missing binary path produces
// a clear error rather than a panic or a silent no-op.
func TestIsolator_WorkerBinMissing(t *testing.T) {
	iso := &ProcessIsolator{
		WorkerBin: "/no/such/distill-worker-binary",
		Timeout:   5 * time.Second,
	}
	_, err := iso.Convert(context.Background(), []byte("hello"), FormatText)
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
}
