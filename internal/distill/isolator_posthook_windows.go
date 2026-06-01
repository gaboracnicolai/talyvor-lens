//go:build windows

package distill

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// applyJobLimit assigns a Windows Job Object to the live worker process
// immediately after cmd.Start(), capping its committed memory at limitBytes.
//
// On Windows, the worker cannot set its own memory ceiling (there is no
// per-process syscall equivalent to RLIMIT_AS that a process can call on
// itself).  Instead, the parent creates a Job Object, sets
// JOB_OBJECT_LIMIT_PROCESS_MEMORY via SetInformationJobObject, and assigns
// the child process to it.  The Job Object persists in the kernel until all
// processes it contains have exited and all handles are closed; closing our
// handle here does not release the child from the constraint.
func applyJobLimit(cmd *exec.Cmd, limitBytes uint64) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("distill: CreateJobObject: %w", err)
	}
	// Close our handle when done — the child remains in the job and the limit
	// stays in effect until the child exits.
	defer windows.CloseHandle(job)

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
	info.ProcessMemoryLimit = uintptr(limitBytes)

	if _, setErr := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); setErr != nil {
		return fmt.Errorf("distill: SetInformationJobObject: %w", setErr)
	}

	// Open a handle to the child process by PID.  There is a narrow race
	// between cmd.Start() and OpenProcess: if the worker has already exited
	// (e.g. immediate error), OpenProcess returns an error which we surface as
	// a non-fatal condition — GOMEMLIMIT still provides soft protection.
	//
	// PROCESS_SET_QUOTA is required for AssignProcessToJobObject.
	// PROCESS_TERMINATE is included so the parent can kill a runaway worker
	// if needed.  PROCESS_ALL_ACCESS is deliberately avoided — least privilege.
	proc, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("distill: OpenProcess pid %d: %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(proc)

	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		return fmt.Errorf("distill: AssignProcessToJobObject: %w", err)
	}
	return nil
}
