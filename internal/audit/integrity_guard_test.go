package audit

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ledgerMut matches an UPDATE/DELETE against the supply-bearing ledgers. These are
// derive-from-full-history tables — mining.LedgerStore.GetTotalSupply SUMs
// lens_token_ledger over ALL history (cache_mining.go), feeding the peg and
// circulating supply — so ANY mutation (a retention DELETE included) silently
// corrupts reconciliation. This invariant is pinned to ZERO in production code,
// forever. (The 0055 triggers enforce it in the DB; this enforces it in the source
// so retention can never be pointed at a ledger.)
var ledgerMut = regexp.MustCompile(`(?i)(UPDATE\s+(lens_token_ledger|lxc_ledger)\b|DELETE\s+FROM\s+(lens_token_ledger|lxc_ledger)\b)`)

// TestAuditIntegrity_NoLedgerMutationInProductionCode — the ledger-retention
// invariant, pinned forever: no UPDATE/DELETE against lens_token_ledger or
// lxc_ledger anywhere in internal/ or cmd/ production code.
func TestAuditIntegrity_NoLedgerMutationInProductionCode(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "sdk":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		sp := filepath.ToSlash(p)
		if !strings.Contains(sp, "/internal/") && !strings.Contains(sp, "/cmd/") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if hit := ledgerMut.FindString(string(src)); hit != "" {
			offenders = append(offenders, p+": "+strings.Join(strings.Fields(hit), " "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("LEDGER MUTATION in production code — breaks GetTotalSupply reconciliation; ledgers are never retention-eligible: %s", o)
	}
}

// TestAuditIntegrity_RetentionBypassFlagSingleCaller — the append-only guarantee
// has exactly ONE scoped exception: the retention sweeper sets
// `SET LOCAL lens.audit_retention = 'on'` so the 0055 trigger permits its
// token_events DELETE. NOTHING else may set that flag, or the immutability promise
// is hollow. Asserts the flag is referenced in EXACTLY ONE non-test file — the
// sweeper. A second file (a new caller) fails. File granularity: the sweeper file
// legitimately names the flag in both a doc comment and the SET LOCAL statement.
func TestAuditIntegrity_RetentionBypassFlagSingleCaller(t *testing.T) {
	const flag = "lens.audit_retention"
	var files []string
	err := filepath.WalkDir("../..", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "sdk":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		sp := filepath.ToSlash(p)
		if !strings.Contains(sp, "/internal/") && !strings.Contains(sp, "/cmd/") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), flag) {
			files = append(files, sp)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("the retention bypass flag %q must be referenced in EXACTLY ONE non-test file (the sweeper); found %d: %v", flag, len(files), files)
	}
	if !strings.HasSuffix(files[0], "internal/audit/retention.go") {
		t.Errorf("the only flag reference must be the retention sweeper (internal/audit/retention.go); got %s", files[0])
	}
}

// TestAuditIntegrity_MigrationGuardsExactlyTheFive — 0055 guards the five
// append-only tables and NOT the two deliberately-mutable ones.
func TestAuditIntegrity_MigrationGuardsExactlyTheFive(t *testing.T) {
	src, err := os.ReadFile("../../migrations/0055_audit_immutability.sql")
	if err != nil {
		t.Fatalf("read 0055: %v", err)
	}
	s := string(src)
	for _, tbl := range []string{"token_events", "lens_token_ledger", "lxc_ledger", "request_attribution", "povi_receipts"} {
		if !strings.Contains(s, "'"+tbl+"'") {
			t.Errorf("0055 must guard %s (missing from the guarded[] array)", tbl)
		}
	}
	for _, want := range []string{"audit_block_mutation", "BEFORE UPDATE OR DELETE", "BEFORE TRUNCATE", "audit_no_mutation", "audit_no_truncate"} {
		if !strings.Contains(s, want) {
			t.Errorf("0055 must contain %q", want)
		}
	}
	for _, tbl := range []string{"lxc_purchases", "pool_royalty_mints"} {
		if strings.Contains(s, "'"+tbl+"'") {
			t.Errorf("%s must NOT be guarded (it has legitimate state transitions)", tbl)
		}
	}
}
