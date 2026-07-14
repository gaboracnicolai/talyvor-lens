package mining

import "testing"

// HaircutFloor is an UNCALIBRATED N3 placeholder. Pin it so the default-on sweep can't have moved the
// money-path bound: a drifting contributor always keeps at least this fraction.
func TestHaircutFloor_Placeholder(t *testing.T) {
	if HaircutFloor != 0.5 {
		t.Errorf("HaircutFloor = %v, want 0.5 (uncalibrated placeholder — recalibrate at N3, not on a flag flip)", HaircutFloor)
	}
}
