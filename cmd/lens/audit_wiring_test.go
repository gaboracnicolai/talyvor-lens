package main

import (
	"os"
	"testing"
)

// TestAuditJobs_LeaderGated — U14: the token_events retention sweeper and the
// off-box export loop must be wired under haComps.leader.Run (exactly one instance
// runs each), like the other singleton background jobs.
//
// ⚠ THIS ASKED THE QUESTION OF A LINE OF TEXT UNTIL #516 AND SO COULD NOT TELL A
// RUNNING JOB FROM A COMMENT: a line containing both `haComps.leader.Run` and the
// quoted job name satisfied it, so commenting the registration out — the ordinary
// way to disable a job — left this green while the sweeper did not run, and a plain
// `go auditRetention.StartSweeper(ctx, …)` with no leader election at all passed
// too, which is the exact opposite of "exactly one instance". Registration is now
// read from main.go's AST by scanLeaderJobs; comments and string literals are not
// in the AST. Arms and verdicts: ~/talyvor-queue/w61-auditwiring-mutation-controls-h2r7.py.
func TestAuditJobs_LeaderGated(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	jobs, err := scanLeaderJobs("main.go", src)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	// Vacuity floor: a scanner that silently found nothing would pass every
	// "must be gated" assertion below by finding no job to complain about.
	// 35 leader-gated singletons measured at #516; the floor is deliberately loose.
	if len(jobs) < 20 {
		t.Fatalf("scanLeaderJobs found only %d leader.Run registrations in main.go — "+
			"the scanner is blind, not the file empty (35 measured at #516)", len(jobs))
	}
	for _, want := range []string{"audit-retention", "audit-export"} {
		n := 0
		for _, j := range jobs {
			if j.name == want {
				n++
			}
		}
		if n == 0 {
			t.Errorf("audit job %q is not started via haComps.leader.Run — it is either not "+
				"registered at all, or registered as a plain goroutine, in which case EVERY "+
				"replica runs it (leader-only singleton)", want)
		}
	}
}
