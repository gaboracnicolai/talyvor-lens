package main

import (
	"os"
	"strings"
	"testing"
)

// TestAuditJobs_LeaderGated — U14: the token_events retention sweeper and the
// off-box export loop must be wired under haComps.leader.Run (exactly one instance
// runs each), like the other singleton background jobs. Structural proof over
// main.go (mirrors TestEconomyKillSwitch_WorkersGuarded).
func TestAuditJobs_LeaderGated(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	for _, job := range []string{`"audit-retention"`, `"audit-export"`} {
		found := false
		for _, ln := range lines {
			if strings.Contains(ln, "haComps.leader.Run") && strings.Contains(ln, job) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("audit job %s must be started via haComps.leader.Run (leader-only singleton)", job)
		}
	}
}
