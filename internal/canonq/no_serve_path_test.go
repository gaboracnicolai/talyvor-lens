package canonq

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ⚠ W2.6's LAST LINE IS AN INSTRUCTION WITH NO ENFORCEMENT ANYWHERE ELSE: "DO NOT SHIP A SERVE
// PATH IN THIS ITEM. Measure, report the two numbers, stop." An instruction that lives only in a
// queue file is obeyed by the session that read it and by nobody afterwards. This test is that
// instruction expressed as a property of the source tree.
//
// ⚠ IT IS DELIBERATELY EXPIRABLE. When the numbers below justify a serve path, this test is what
// has to be deleted to build one — and deleting it is a visible, reviewable act, which is the
// whole point. It is not a claim that a serve path would be wrong forever.

// servePaths are the packages a pooled read actually runs through. Named individually rather than
// scanned wholesale so that a NEW package cannot quietly inherit the exemption.
var servePaths = []string{
	"../proxy",
	"../cache",
	"../discriminator",
}

func TestCanonqIsNotOnAnyServePath(t *testing.T) {
	read := 0
	for _, dir := range servePaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("cannot read %s: %v — a guard that read nothing reports the same zero as a clean tree", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			p := filepath.Join(dir, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("cannot read %s: %v", p, err)
			}
			read++
			if strings.Contains(string(b), "lens/internal/canonq") || strings.Contains(string(b), "canonq.") {
				t.Errorf("%s references canonq — W2.6 says MEASURE AND STOP; a serve path is a separate, argued decision", p)
			}
		}
	}

	// ⚠ THE FLOOR. Zero files read is not "no serve path references canonq", it is "this test
	// did not run". A moved package or a renamed directory would otherwise report clean.
	if read < 20 {
		t.Fatalf("read only %d .go files across %v — too few for this to be a measurement", read, servePaths)
	}
}

// ⚠ AND THE FLOOR ABOVE CANNOT SEE A BLIND NEEDLE. `read >= 20` proves files were opened; it says
// nothing about whether the substring test can match anything at all. This asserts the needle
// mechanism against a string those same files DO contain, so a typo in the needle above is caught
// by the same run.
func TestServePathScanCanActuallyMatch(t *testing.T) {
	hits := 0
	for _, dir := range servePaths {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if strings.Contains(string(b), "lens/internal/discriminator") {
				hits++
			}
		}
	}
	if hits == 0 {
		t.Fatal("the scan matched zero files on a control needle that IS present — the needle test is dead, so the canonq scan above proves nothing")
	}
}
