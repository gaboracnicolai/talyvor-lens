package audit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// THE RETENTION SWEEPER DELETED ROWS THAT WERE NOT OLD, ON AN APPEND-ONLY TABLE.
//
// deleteAgedSQL selected the victims by `ctid`:
//
//	DELETE FROM token_events WHERE ctid IN (SELECT ctid FROM token_events WHERE created_at < $1 LIMIT $2)
//
// ⚠ ctid IS UNIQUE WITHIN A PHYSICAL RELATION, NOT WITHIN A PARTITIONED TABLE.
// Migration 0034 turned token_events into 8 HASH partitions on workspace_id, so
// two rows in two different partitions routinely carry the SAME ctid — the first
// row of every partition is (0,1). The inner SELECT reads ctids from every
// partition and the outer DELETE compares them against every partition, so an
// AGED row in partition p2 nominates ctid (0,1) and the DELETE also removes the
// BRAND-NEW row that happens to sit at (0,1) in partition p4.
//
// ⚠⚠ WHAT IS DESTROYED IS THE THING THE TABLE EXISTS TO GUARANTEE. token_events
// is append-only under the 0055 triggers precisely so audit history cannot be
// rewritten, and this sweeper holds the ONLY key to that door (`SET LOCAL
// lens.audit_retention = 'on'`). A row inside its retention window — including,
// under the export-gated variant, a row that has NOT yet been exported off-box,
// which is the whole point of proof-of-export-before-delete — is deleted with no
// error, no log line and no way to tell afterwards.
//
// ⚠ WHY NOTHING SAW IT FOR 118 MIGRATIONS: whether a collision happens is a
// property of PHYSICAL LAYOUT, not of the data. The existing retention tests seed
// each case into ONE workspace, so every row of a case lands in ONE partition
// where ctids really are unique, and the suite is green by accident of layout.
// It reproduces the moment any other row exists in another partition — which is
// how it was found: adding an unrelated test that wrote four rows to a different
// workspace turned TestRetention_RequireExportOff_AgeOnly red, reporting that its
// one-hour-old row had been swept by a thirty-day window.
//
// The fix is the PRIMARY KEY, which migration 0034 declares as (id, workspace_id)
// and which is unique across the whole partitioned table by construction.

// partitionOf reports which partition a row landed in, so the test can PROVE it
// built the collision it claims to be testing rather than assuming the hash.
func partitionOf(t *testing.T, pool *pgxpool.Pool, ws string) (part, ctid string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT tableoid::regclass::text, ctid::text FROM token_events WHERE workspace_id = $1`, ws,
	).Scan(&part, &ctid); err != nil {
		t.Fatalf("read partition/ctid for %s: %v", ws, err)
	}
	return part, ctid
}

// TestRetention_DoesNotSweepAcrossPartitionsOnACollidingCtid builds the exact
// collision — an AGED row and a FRESH row in two different partitions sharing one
// ctid — and requires the fresh row to survive a sweep that should only take the
// aged one.
//
// ⚠ THE TEST ASSERTS ITS OWN PREMISE FIRST. If the two workspaces ever hash to
// the same partition, or the ctids differ, the collision does not exist and a
// green result would prove nothing — so that case fails loudly as a broken
// instrument rather than passing as a healthy product.
func TestRetention_DoesNotSweepAcrossPartitionsOnACollidingCtid(t *testing.T) {
	pool := auditTestPool(t)
	ctx := context.Background()

	// Chosen by measurement, not by guesswork: these two workspace ids hash to
	// DIFFERENT partitions of the 8-way HASH partitioning in 0034. The assertion
	// below is what keeps that true if the modulus ever changes.
	const wsAged = "ctid_collide_aged"
	const wsFresh = "ctid_collide_fresh"

	drainAndSeed := func() {
		// Start from a table with no other rows so each seeded row is the FIRST
		// physical row of its own partition and therefore lands at ctid (0,1).
		purgeAllTokenEvents(t, pool)
		seedTokenEventAt(t, pool, wsAged, 60*24*time.Hour)
		seedTokenEventAt(t, pool, wsFresh, 1*time.Hour)
	}
	drainAndSeed()

	agedPart, agedCtid := partitionOf(t, pool, wsAged)
	freshPart, freshCtid := partitionOf(t, pool, wsFresh)
	if agedPart == freshPart {
		t.Fatalf("INSTRUMENT BROKEN, not a product result: %q and %q landed in the SAME partition (%s), "+
			"so there is no cross-partition ctid collision to test. Pick two workspace ids that hash apart.",
			wsAged, wsFresh, agedPart)
	}
	if agedCtid != freshCtid {
		t.Fatalf("INSTRUMENT BROKEN, not a product result: the two rows have DIFFERENT ctids "+
			"(%s in %s vs %s in %s), so this test is not exercising the collision it is named for.",
			agedCtid, agedPart, freshCtid, freshPart)
	}
	t.Logf("collision built: %s@%s and %s@%s both at ctid %s", wsAged, agedPart, wsFresh, freshPart, agedCtid)

	// Age-only sweep, 30-day window: the 60-day row must go, the 1-hour row must stay.
	r := NewRetention(pool, 30*24*time.Hour, false, false)
	if _, err := r.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if c := countWS(t, pool, wsAged); c != 0 {
		t.Errorf("the AGED row should have been swept; %d remain", c)
	}
	if c := countWS(t, pool, wsFresh); c != 1 {
		t.Fatalf("DATA LOSS: the 1-hour-old row was deleted by a 30-DAY retention sweep because it "+
			"shared ctid %s with an aged row in a different partition (%s vs %s). ctid is unique per "+
			"PHYSICAL RELATION; token_events is 8-way HASH partitioned (migration 0034), so the "+
			"sweeper's `WHERE ctid IN (SELECT ctid ...)` matches across partitions. This is silent "+
			"destruction of audit history on the table the 0055 append-only triggers exist to protect.",
			agedCtid, agedPart, freshPart)
	}

	// ⚠ AND THE SAME COLLISION UNDER THE EXPORT-GATED VARIANT, which is the worse
	// half: there the fresh row may also be UN-EXPORTED, so a cross-partition
	// delete destroys the very row proof-of-export-before-delete promises to keep.
	drainAndSeed()
	setWatermark(t, pool, time.Now().Format(time.RFC3339))
	rx := NewRetention(pool, 30*24*time.Hour, true, true)
	if _, err := rx.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce (export-gated): %v", err)
	}
	if c := countWS(t, pool, wsFresh); c != 1 {
		t.Fatalf("DATA LOSS under the EXPORT-GATED sweep: the fresh row was deleted via a colliding " +
			"ctid. proof-of-export-before-delete cannot hold if the delete does not identify rows uniquely.")
	}
	purgeAllTokenEvents(t, pool)
}

// seedTokenEventAt inserts one row of a given age, binding the same never-NULL
// string columns alerts.go binds (see retention_integration_test.go#seedTokenEvent
// for why omitting them poisons the export tests).
func seedTokenEventAt(t *testing.T, pool *pgxpool.Pool, ws string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO token_events
		   (provider, model, input_tokens, output_tokens, workspace_id, created_at,
		    team, feature, cost_usd, pii_detected, session_id, request_id)
		 VALUES ('p','m',1,1,$1,$2, '', '', 0, false, '', gen_random_uuid()::text)`,
		ws, time.Now().Add(-age)); err != nil {
		t.Fatalf("seed token_events: %v", err)
	}
}

// purgeAllTokenEvents empties the table through the ONLY sanctioned door — the
// same transaction-local flag retention.go#deleteBatch uses. It leaves the
// package's shared table as it found it, so this test cannot poison a sibling.
func purgeAllTokenEvents(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("purge begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lens.audit_retention = 'on'`); err != nil {
		t.Fatalf("purge set flag: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_events`); err != nil {
		t.Fatalf("purge delete: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("purge commit: %v", err)
	}
	// ⚠ VACUUM IS LOAD-BEARING, NOT HOUSEKEEPING. A DELETE leaves dead tuples, so
	// the next INSERT lands AFTER them and the first row of a partition is no
	// longer (0,1) — which is how this test first ran: sibling tests had already
	// written to p4, the aged row landed at (0,5), the collision was never built
	// and the premise assertion (correctly) refused to call that a healthy
	// product. Reclaiming the space is what makes the collision reproducible
	// rather than dependent on what ran before.
	if _, err := pool.Exec(ctx, `VACUUM `+retentionTable); err != nil {
		t.Fatalf("purge vacuum: %v", err)
	}
}
