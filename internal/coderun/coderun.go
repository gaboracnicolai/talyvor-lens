// Package coderun is Lens's sandbox for code a model runs while it answers (B28.119): Chat's Run code.
//
// The code is JavaScript, run by an interpreter (goja) that has no file, network, process or clock access to give
// it — only console.log. It runs in a child process of Lens (the same binary, ChildArg), started with an empty
// environment so no key or setting reaches it, and stopped on the clock, on memory and on output. The process is
// there so a run that eats memory or crashes takes only itself down, never the gateway.
package coderun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// ChildArg is the subcommand cmd/lens runs ChildMain for. Nothing else starts it.
const ChildArg = "coderun-child"

// Language is what the sandbox runs, as the stream's code_run frame names it.
const Language = "javascript"

const (
	maxCode    = 64 << 10        // a longer program is refused before it runs
	maxStdout  = 16 << 10        // what a run printed, kept for the model and the screen
	maxStderr  = 4 << 10         // its error
	maxHeap    = 256 << 20       // a run holding more is stopped
	runFor     = 5 * time.Second // the interpreter stops the code itself
	killAfter  = 10 * time.Second
	concurrent = 4 // runs at once; a fifth waits its turn
)

// Result is one run: the code, what it printed and how it ended — the fields of the code_run frame.
type Result struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// Runner starts each run in a child process of exe.
type Runner struct {
	exe   string
	slots chan struct{}
}

// NewRunner runs code in children of this very binary, which must call ChildMain for ChildArg.
func NewRunner() (*Runner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Runner{exe: exe, slots: make(chan struct{}, concurrent)}, nil
}

// Run runs code in the sandbox. It never returns an error: a sandbox that could not run the code says so in Stderr,
// which is what the model and the screen are shown.
func (r *Runner) Run(ctx context.Context, code string) Result {
	res := Result{Language: Language, Code: code}
	if len(code) > maxCode {
		res.Stderr, res.ExitCode = "The code is longer than the sandbox runs (64 KB).", 1
		return res
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		res.Stderr, res.ExitCode = "The request ended before the code ran.", -1
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, killAfter)
	defer cancel()
	//nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	cmd := exec.CommandContext(cctx, r.exe, ChildArg) // our own binary; the untrusted code goes in on stdin, never argv
	cmd.Env = []string{}                              // empty, not nil: nil would hand the child Lens's whole environment
	cmd.Stdin = strings.NewReader(code)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var got Result
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr == nil && out.Len() > 0 {
		res.Stdout, res.Stderr, res.ExitCode, res.TimedOut = got.Stdout, got.Stderr, got.ExitCode, got.TimedOut
		return res
	}
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode, res.Stderr = true, -1, "Stopped: it ran too long."
	case err != nil:
		res.ExitCode, res.Stderr = -1, "The sandbox stopped it (out of memory, or it crashed)."
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			res.ExitCode = ee.ExitCode()
		}
	default:
		res.ExitCode, res.Stderr = -1, "The sandbox gave back no result."
	}
	return res
}

// ChildMain runs the program on stdin and writes its Result as JSON on stdout. cmd/lens calls it for ChildArg.
func ChildMain() {
	runtime.GOMAXPROCS(1)
	debug.SetMemoryLimit(maxHeap)
	// Linux: if memory runs out anyway, the kernel kills this process, not Lens. Elsewhere the file is absent.
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0)
	limitAddressSpace()
	code, _ := io.ReadAll(io.LimitReader(os.Stdin, maxCode+1))
	res := Eval(string(code), runFor)
	b, _ := json.Marshal(res)
	_, _ = os.Stdout.Write(b)
	os.Exit(0)
}

// cappedBuffer keeps the first max bytes written to it and drops the rest, saying so once.
type cappedBuffer struct {
	b   strings.Builder
	max int
	cut bool
}

func (c *cappedBuffer) write(s string) {
	if room := c.max - c.b.Len(); len(s) > room {
		if !c.cut {
			c.b.WriteString(s[:max(room, 0)])
			c.b.WriteString("\n… (output cut short)\n")
			c.cut = true
		}
		return
	}
	c.b.WriteString(s)
}

// Eval runs code in a fresh interpreter for at most d, in this process. Only ChildMain and tests call it: Lens
// itself always goes through Runner, so a run never shares the gateway's memory.
func Eval(code string, d time.Duration) Result {
	res := Result{Language: Language, Code: code}
	vm := goja.New()
	vm.SetMaxCallStackSize(10_000)
	stdout := &cappedBuffer{max: maxStdout}
	stderr := &cappedBuffer{max: maxStderr}
	show := func(v goja.Value) string {
		if o, ok := v.(*goja.Object); ok && o.ClassName() != "Function" && o.ClassName() != "Error" {
			if b, err := json.Marshal(o.Export()); err == nil {
				return string(b)
			}
		}
		return v.String()
	}
	printer := func(to *cappedBuffer) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, a := range call.Arguments {
				parts[i] = show(a)
			}
			to.write(strings.Join(parts, " ") + "\n")
			return goja.Undefined()
		}
	}
	console := vm.NewObject()
	for name, to := range map[string]*cappedBuffer{"log": stdout, "info": stdout, "error": stderr, "warn": stderr} {
		_ = console.Set(name, printer(to))
	}
	_ = vm.Set("console", console)
	_ = vm.Set("print", printer(stdout))

	stop := time.AfterFunc(d, func() { vm.Interrupt("timeout") })
	// A program that grows past maxHeap is stopped too; the check is cheap next to what a run allocates.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if runtime.ReadMemStats(&m); m.HeapAlloc > maxHeap {
					vm.Interrupt("memory")
					return
				}
			}
		}
	}()
	v, err := vm.RunString(code)
	stop.Stop()
	close(done)

	var interrupted *goja.InterruptedError
	var thrown *goja.Exception
	switch {
	case errors.As(err, &interrupted) && interrupted.Value() == "timeout":
		res.TimedOut, res.ExitCode = true, -1
		stderr.write("Stopped: it ran longer than " + d.String() + ".\n")
	case errors.As(err, &interrupted):
		res.ExitCode = 137
		stderr.write("Stopped: it used more than 256 MB of memory.\n")
	case errors.As(err, &thrown):
		res.ExitCode = 1
		stderr.write(thrown.Error() + "\n")
	case err != nil: // a syntax error
		res.ExitCode = 1
		stderr.write(err.Error() + "\n")
	case stdout.b.Len() == 0 && v != nil && !goja.IsUndefined(v) && !goja.IsNull(v):
		// Nothing printed: show the program's last value, as a console would.
		stdout.write(show(v) + "\n")
	}
	res.Stdout, res.Stderr = stdout.b.String(), stderr.b.String()
	return res
}
