package poolroyalty

import (
	"regexp"
	"strings"
	"testing"

	"github.com/talyvor/lens/migrations"
)

// Stage-2.1 schema invariants, asserted against the embedded migrations so a
// future migration can't silently break them:
//
//  1. pool_royalty_mints carries UNIQUE(request_id) — the idempotency key —
//     and stays UNPARTITIONED (a bare UNIQUE on one column is illegal on the
//     hash-partitioned hot tables, whose PK must include workspace_id).
//  2. token_events never gains a UNIQUE constraint/index on request_id — the
//     dedup lives on the unpartitioned claim table, NOT on the partitioned
//     hot-write table (where it would be illegal anyway: any UNIQUE there
//     must include the partition key, and a cross-tenant hit has TWO
//     workspaces, so neither choice would be right).
//  3. No migration takes a session-scoped advisory lock: pg_advisory_lock is
//     incompatible with PgBouncer transaction pooling; only the
//     transaction-scoped pg_advisory_xact_lock is permitted repo-wide.
func TestMigrations_PoolRoyaltySchemaInvariants(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}

	var claimMigration string
	uniqueOnTokenEvents := regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX[^;]*\bON\s+token_events\b[^;]*\brequest_id\b`)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		sql := string(raw)

		if strings.Contains(sql, "pool_royalty_mints") {
			claimMigration += sql // accumulate in case of follow-up migrations
		}
		if uniqueOnTokenEvents.MatchString(sql) {
			t.Errorf("%s puts a UNIQUE index on token_events(request_id) — the idempotency key must live on the unpartitioned claim table", e.Name())
		}
		if strings.Contains(sql, "token_events") {
			// Inline UNIQUE on the request_id column inside a token_events
			// CREATE TABLE would be equally illegal/wrong.
			for _, line := range strings.Split(sql, "\n") {
				l := strings.ToLower(line)
				if strings.Contains(l, "request_id") && strings.Contains(l, "unique") {
					t.Errorf("%s declares request_id UNIQUE in a token_events context: %q", e.Name(), strings.TrimSpace(line))
				}
			}
		}
		if strings.Contains(strings.ToLower(sql), "pg_advisory_lock(") {
			t.Errorf("%s takes a SESSION-scoped advisory lock — only pg_advisory_xact_lock is PgBouncer-transaction-pooling safe", e.Name())
		}
	}

	if claimMigration == "" {
		t.Fatal("no migration defines pool_royalty_mints")
	}

	// Stage 2.2: the margin read surface must stay a DERIVATION over the claim
	// rows (margin_usd = avoided_cogs_usd − minted_amount) and must never
	// write to or read from token_events — margin is revenue, token_events is
	// customer spend, and the spend readers have no row-type filter.
	viewRaw, err := migrations.FS.ReadFile("0044_pool_royalty_margin_view.sql")
	if err != nil {
		t.Fatalf("read 0044 margin view migration: %v", err)
	}
	viewSQL := string(viewRaw)
	// Re-run-safe form is DROP IF EXISTS + CREATE, NOT "CREATE OR REPLACE":
	// 0046 later appends a status column to this view, and OR REPLACE cannot
	// drop columns — a full-chain replay would die at 0044 with 42P16 (#127).
	if !regexp.MustCompile(`(?i)DROP\s+VIEW\s+IF\s+EXISTS\s+pool_royalty_margin\s*;\s*CREATE\s+VIEW\s+pool_royalty_margin`).MatchString(viewSQL) {
		t.Error("0044 must DROP VIEW IF EXISTS + CREATE VIEW pool_royalty_margin (the re-run-safe form; OR REPLACE dies on the 0046 column append during a full replay)")
	}
	if !regexp.MustCompile(`(?i)avoided_cogs_usd\s*-\s*minted_amount\s+AS\s+margin_usd`).MatchString(viewSQL) {
		t.Error("the margin view must DERIVE margin_usd = avoided_cogs_usd - minted_amount (the margin identity) — never store it")
	}
	for _, line := range strings.Split(viewSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // comments may (and do) EXPLAIN why token_events is avoided
		}
		if strings.Contains(strings.ToLower(trimmed), "token_events") {
			t.Errorf("the margin view must not touch token_events (customer spend): %q", trimmed)
		}
	}
	if !regexp.MustCompile(`(?i)request_id\s+TEXT\s+NOT\s+NULL\s+UNIQUE`).MatchString(claimMigration) {
		t.Error("pool_royalty_mints must declare request_id TEXT NOT NULL UNIQUE — the exactly-once idempotency key")
	}
	if strings.Contains(strings.ToUpper(claimMigration), "PARTITION BY") {
		t.Error("pool_royalty_mints must stay UNPARTITIONED: partitioning would force the UNIQUE key to include the partition key, breaking UNIQUE(request_id)")
	}

	// Stage 2.3.0: the serve-time evidence hashes. Both columns must exist,
	// additive with DEFAULT '' so pre-2.3.0 rows stay readable ('' = historical
	// "not captured"; the no-hash-no-mint gate applies only to NEW serves, in
	// the write path — never to existing rows).
	for _, col := range []string{"answer_sha256", "prompt_sha256"} {
		re := regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+` + col + `\s+TEXT\s+NOT\s+NULL\s+DEFAULT\s+''`)
		if !re.MatchString(claimMigration) {
			t.Errorf("pool_royalty_mints must gain %s via additive ADD COLUMN IF NOT EXISTS ... TEXT NOT NULL DEFAULT '' (Stage 2.3.0 evidence hash)", col)
		}
	}

	// Stage 2.3a: the holdback columns. status defaults 'final' so every
	// pre-2.3a row is correctly grandfathered (they were minted straight to
	// spendable); finalize_after is nullable (final rows have no window);
	// the sweep gets a partial index over the small transient held set; and
	// held_balance is a NEW column with the 0032/0036 idiom — NEVER the
	// locked_balance collateral column.
	if !regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+status\s+TEXT\s+NOT\s+NULL\s+DEFAULT\s+'final'`).MatchString(claimMigration) {
		t.Error("pool_royalty_mints must gain status TEXT NOT NULL DEFAULT 'final' (grandfathers pre-2.3a rows)")
	}
	if !regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+finalize_after\s+TIMESTAMPTZ`).MatchString(claimMigration) {
		t.Error("pool_royalty_mints must gain finalize_after TIMESTAMPTZ (nullable; NULL on final rows)")
	}
	if !regexp.MustCompile(`(?is)CREATE\s+INDEX\s+IF\s+NOT\s+EXISTS[^;]*ON\s+pool_royalty_mints\s*\(finalize_after\)[^;]*WHERE\s+status\s*=\s*'held'`).MatchString(claimMigration) {
		t.Error("the finalize sweep needs the partial index ON pool_royalty_mints (finalize_after) WHERE status = 'held'")
	}

	var heldMigration string
	entries2, _ := migrations.FS.ReadDir(".")
	for _, e := range entries2 {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, _ := migrations.FS.ReadFile(e.Name())
		if strings.Contains(string(raw), "held_balance") {
			heldMigration += string(raw)
		}
	}
	if !regexp.MustCompile(`(?i)ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS\s+held_balance\s+DOUBLE\s+PRECISION\s+NOT\s+NULL\s+DEFAULT\s+0`).MatchString(heldMigration) {
		t.Error("lens_token_balances must gain held_balance DOUBLE PRECISION NOT NULL DEFAULT 0 (the 0032 idiom; never co-tenant locked_balance)")
	}
	if !regexp.MustCompile(`(?i)CHECK\s*\(held_balance\s*>=\s*0\)\s*NOT\s+VALID`).MatchString(heldMigration) {
		t.Error("held_balance needs the 0036-style CHECK (held_balance >= 0) NOT VALID")
	}

	// Per-entry cap (2.3b follow-up): the hot-path entry_id COUNT REQUIRES an
	// (entry_id, created_at) index — without it every mint seq-scans. Pinned
	// here so it can't be dropped.
	var entryIdxMigration string
	entries3, _ := migrations.FS.ReadDir(".")
	for _, e := range entries3 {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, _ := migrations.FS.ReadFile(e.Name())
		if strings.Contains(string(raw), "idx_pool_royalty_mints_entry") {
			entryIdxMigration += string(raw)
		}
	}
	if !regexp.MustCompile(`(?is)CREATE\s+INDEX\s+IF\s+NOT\s+EXISTS\s+idx_pool_royalty_mints_entry\s+ON\s+pool_royalty_mints\s*\(entry_id,\s*created_at\)`).MatchString(entryIdxMigration) {
		t.Error("the per-entry cap requires CREATE INDEX IF NOT EXISTS idx_pool_royalty_mints_entry ON pool_royalty_mints (entry_id, created_at) — hot-path COUNT must not seq-scan")
	}

	// Stage-3 adjudication record (0048): the audit row binding record→revoke.
	// Additive new table; TEXT[] capture-at-decision sets; outcome JSONB
	// (nullable until the revoke completes — the record-before-burn ordering).
	var adjMigration string
	entries4, _ := migrations.FS.ReadDir(".")
	for _, e := range entries4 {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, _ := migrations.FS.ReadFile(e.Name())
		if strings.Contains(string(raw), "pool_royalty_adjudications") {
			adjMigration += string(raw)
		}
	}
	if !regexp.MustCompile(`(?is)CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+pool_royalty_adjudications`).MatchString(adjMigration) {
		t.Error("0048 must CREATE TABLE IF NOT EXISTS pool_royalty_adjudications (the audit record binding record→revoke)")
	}
	for _, col := range []string{"flag_type", "resolution_label", "candidate_request_ids", "revoked_request_ids", "decided_by", "outcome", "decided_at"} {
		if !regexp.MustCompile(`(?i)\b` + col + `\b`).MatchString(adjMigration) {
			t.Errorf("pool_royalty_adjudications missing column %s", col)
		}
	}
	if !regexp.MustCompile(`(?i)candidate_request_ids\s+TEXT\[\]`).MatchString(adjMigration) {
		t.Error("candidate_request_ids must be TEXT[] (audit snapshot, captured at decision)")
	}
	if !regexp.MustCompile(`(?i)revoked_request_ids\s+TEXT\[\]`).MatchString(adjMigration) {
		t.Error("revoked_request_ids must be TEXT[]")
	}
	if !regexp.MustCompile(`(?i)outcome\s+JSONB`).MatchString(adjMigration) {
		t.Error("outcome must be JSONB (the RevokeReport, nullable until the revoke completes)")
	}
}
