package dbmigrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// MIGRATIONS ARE APPEND-ONLY, SO A WRONG RATIONALE IN ONE IS PERMANENT.
//
// Two migrations (0100, 0101) justified a column with a business model that did not exist: the cache
// hit rate "prices BYOK" and "tests whether our margin is the cache". BYOK is not built and is
// deliberately not being built yet; there is no subscription for a cache hit to retain margin on. The
// code, README and COORDINATION.md were all corrected when the economics were fixed — and these two
// files were missed, because nobody re-reads an applied migration. They are read, though: by whoever
// inherits this and needs to know why a column exists.
//
// A comment cannot be fixed by a later migration the way a schema can. So this guard exists to stop
// the assumption being reintroduced by a future author who copies the framing from an older file.
//
// THE RULE: a banned phrase may appear in a migration ONLY in a file that also carries a correction
// note. That lets the corrections quote the wrong text — a reader has to see what was wrong to trust
// the fix — while a NEW file that states it as fact fails.
func TestMigrationComments_NoRetiredEconomicModel(t *testing.T) {
	// Phrases that assert the retired model as fact. Lowercased before matching.
	banned := []struct{ phrase, why string }{
		{"prices byok", "BYOK is NOT BUILT and deliberately not planned yet — nothing prices it"},
		{"byok subscription", "there is no subscription; billing is per-request against prepaid LXC at a fixed $0.10 peg"},
		{"our margin is the cache", "the inverted model, removed from code/README/COORDINATION.md — a cache hit is not retained margin on a flat fee"},
	}
	const correctionMarker = "comment corrected"

	dir := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		scanned++
		lower := strings.ToLower(string(raw))
		corrected := strings.Contains(lower, correctionMarker)
		for _, b := range banned {
			if !strings.Contains(lower, b.phrase) {
				continue
			}
			if corrected {
				continue // quoting the retired claim inside its own correction is required, not a defect
			}
			t.Errorf("migrations/%s asserts a retired economic model: %q — %s.\n"+
				"Migrations are append-only, so this cannot be fixed by a later migration; correct the "+
				"comment in place (the migrator tracks VERSIONS, not content checksums, so an applied "+
				"migration's comment can be edited safely) and include a \"COMMENT CORRECTED\" note.",
				e.Name(), b.phrase, b.why)
		}
	}
	// Vacuity control: a guard that scanned nothing is indistinguishable from a passing one.
	if scanned == 0 {
		t.Fatal("scanned no migration files — this guard is passing vacuously (wrong path?)")
	}
	if scanned < 100 {
		t.Errorf("scanned only %d migrations; there were 107 at the time this guard was written, so it "+
			"is probably reading the wrong directory", scanned)
	}
}
