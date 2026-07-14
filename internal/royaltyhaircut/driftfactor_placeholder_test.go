package royaltyhaircut

import "testing"

// DriftFactor is an UNCALIBRATED N3 placeholder. Pin it so the default-on sweep can't have moved the reduction
// applied to a hardened-drift workspace.
func TestDriftFactor_Placeholder(t *testing.T) {
	if DriftFactor != 0.5 {
		t.Errorf("DriftFactor = %v, want 0.5 (uncalibrated placeholder — recalibrate at N3, not on a flag flip)", DriftFactor)
	}
}
