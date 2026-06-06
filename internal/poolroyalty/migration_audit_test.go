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
	if !regexp.MustCompile(`(?i)CREATE\s+OR\s+REPLACE\s+VIEW\s+pool_royalty_margin`).MatchString(viewSQL) {
		t.Error("0044 must CREATE OR REPLACE VIEW pool_royalty_margin (idempotent view)")
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
}
