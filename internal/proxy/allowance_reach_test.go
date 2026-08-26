package proxy

// allowance_reach_test.go — W4.6.1 STEP 2b, MEASURED AND NOT BUILT.
//
// ⚠ THE ALLOWANCE LEDGER HAS NO READER. `billing.Service.Consume` has ZERO production callers and
// `billing.Service.CurrentAllowance` has none at all. Step 2 shipped a table, a hard
// CHECK-constrained cap, a row lock and six concurrency controls, and nothing on the serving path
// asks it anything — so a subscription creates a row and changes nothing about what is served or
// what is charged, at any flag setting. Step 2's own note discloses this ("It does NOT gate
// serving") and names the wiring as the natural next merge.
//
// ⚠⚠ AND THERE IS A TRAP IN THAT WIRING THAT COST THIS SESSION AN ABANDONED BRANCH. The obvious
// place to spend an allowance is `shadowSpendLXC` — it is the function whose whole job is
// "post-serve LXC debit", and the LXC admission gate's header names it as the path that books the
// real cost. BUT BOTH ITS CALL SITES SIT IN THE `else` OF `if p.reservationActive()`, AND
// RESERVATIONS ARE ON BY DEFAULT:
//
//	if p.reservationActive() { settleReservation(...) } else { shadowSpendLXC(...) }
//
// `reservationActive()` is `LXCReservationEnabled && agentSpender != nil && LXCAgentAllocationEnabled`,
// and config.Load sets BOTH flags true. So an allowance wired only into shadowSpendLXC would be
// INERT IN THE DEFAULT CONFIGURATION — a money mechanism that looks wired, passes its own unit
// tests, and never runs. That is the same defect class the allowance ledger is already in, one
// layer down.
//
// ⚠ THIS FILE BUILDS NOTHING AND CHANGES NOTHING. It makes both facts executable so the next tab
// starts from a measurement instead of from the obvious-looking half of the change. The brief is
// docs/model2-step2b-brief.md.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMeasured_TheAllowanceLedgerHasNoProductionReader reads the tree.
//
// ⚠ TWO-SIDED. It also requires the ledger's own methods to still EXIST — a census that passes
// because the thing it looks for was deleted is reporting its own blindness, not a finding.
func TestMeasured_TheAllowanceLedgerHasNoProductionReader(t *testing.T) {
	root := "../.."
	var callers []string
	sawDefinition := map[string]bool{}

	err := walkGoFiles(root, func(path string, src string) {
		rel := strings.TrimPrefix(path, root+"/")
		if strings.HasSuffix(rel, "_test.go") {
			return
		}
		if strings.HasPrefix(rel, "internal/billing/") {
			// The ledger's own package: record that the methods are still declared, and do not
			// count their definitions as callers.
			if strings.Contains(src, "func (s *Service) Consume(") {
				sawDefinition["Consume"] = true
			}
			if strings.Contains(src, "func (s *Service) CurrentAllowance(") {
				sawDefinition["CurrentAllowance"] = true
			}
			return
		}
		// ⚠ THE FILTER IS "MENTIONS AN ALLOWANCE", NOT "IMPORTS internal/billing", AND CONTROL G1
		// IS WHY. The first version of this census required the calling file to import
		// internal/billing — which excluded exactly the shape a real wiring takes. A serving-path
		// reader calls the ledger through a LOCAL INTERFACE (so internal/proxy need not import
		// internal/billing for one struct), so that file imports nothing from billing at all and
		// the census reported "no production reader" with the reader sitting in front of it. It
		// would have gone green over the very change it exists to notice.
		//
		// "Mentions an allowance" still excludes internal/attestation's unrelated nonce-store
		// Consume, which is the one false positive this filter exists to remove.
		if !strings.Contains(src, "llowance") {
			return
		}
		for _, m := range []string{".Consume(", "CurrentAllowance(", "RemainingAllowanceULXC(", "SetAllowanceLedger("} {
			if strings.Contains(src, m) {
				callers = append(callers, rel+" -> "+m)
			}
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if !sawDefinition["Consume"] || !sawDefinition["CurrentAllowance"] {
		t.Fatalf("the ledger's own methods are no longer declared where this census looks "+
			"(Consume=%v CurrentAllowance=%v) — re-anchor it, because 'no callers' would then be "+
			"a statement about the SEARCH and not about the tree",
			sawDefinition["Consume"], sawDefinition["CurrentAllowance"])
	}
	if len(callers) > 0 {
		t.Fatalf("the allowance ledger now HAS a production reader:\n  %s\n\n"+
			"⚠ IF THIS IS STEP 2b LANDING, THAT IS THE POINT: delete this test and the brief in "+
			"docs/model2-step2b-brief.md. Check that the reader is on the path that actually runs "+
			"— see TestMeasured_TheDefaultDebitPathIsTheReservationSettleNotTheShadowDebit.",
			strings.Join(callers, "\n  "))
	}
}

// ⚠ THE TRAP, EXECUTABLE. Both shadowSpendLXC call sites are in the `else` of reservationActive().
func TestMeasured_TheDefaultDebitPathIsTheReservationSettleNotTheShadowDebit(t *testing.T) {
	src, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")

	var sites, guarded int
	for i, l := range lines {
		if !strings.Contains(l, "p.shadowSpendLXC(") {
			continue
		}
		sites++
		// The branch opens a few lines above: `if p.reservationActive() { ... } else {`.
		from := i - 6
		if from < 0 {
			from = 0
		}
		window := strings.Join(lines[from:i], "\n")
		if strings.Contains(window, "p.reservationActive()") && strings.Contains(window, "} else {") {
			guarded++
		}
	}

	if sites == 0 {
		t.Fatal("no shadowSpendLXC call sites found in proxy.go — this census has gone blind, and " +
			"a verdict from it would be worthless")
	}
	if guarded != sites {
		t.Fatalf("%d of %d shadowSpendLXC call sites sit in the `else` of reservationActive(). If "+
			"that changed, the trap this file records may be gone — re-read the branch before "+
			"trusting the brief in docs/model2-step2b-brief.md", guarded, sites)
	}
	t.Logf("MEASURED: all %d shadowSpendLXC call sites are the NON-DEFAULT branch. "+
		"reservationActive() = LXCReservationEnabled && agentSpender != nil && "+
		"LXCAgentAllocationEnabled, and config.Load sets both flags true, so an allowance wired "+
		"only into shadowSpendLXC would never run in a default deployment.", sites)
}

// walkGoFiles visits every .go file under root, skipping vendor and testdata.
//
// ⚠ IT WALKS THE TREE RATHER THAN TAKING A LIST. A hand-kept list of "places a caller could be" is
// the defect this census exists to catch, one level up: the caller that matters is the one nobody
// thought to list.
func walkGoFiles(root string, visit func(path, src string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		visit(filepath.ToSlash(path), string(b))
		return nil
	})
}
