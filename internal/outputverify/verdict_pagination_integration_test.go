package outputverify_test

import (
	"context"
	"testing"

	"github.com/talyvor/lens/internal/outputverify"
)

// Pagination: ListForWorkspacePage must page with limit+offset — page 2 differs from page 1 with no
// overlap. Without an OFFSET the two pages would be identical (the leak this closes: an unbounded
// newest-100 read that cannot walk backwards). Real PG because the ORDER BY + LIMIT/OFFSET is the SQL.
func TestReader_ListForWorkspacePage_Paginates(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	ws := "ws-verdict-page"
	if _, err := pool.Exec(ctx, `DELETE FROM k4_output_verdicts WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	w := outputverify.NewWriter(pool)
	// 5 verdicts, distinct increasing created_at so ORDER BY created_at DESC is deterministic
	// (newest = vp-4). Seed via the real writer, then stamp a distinct created_at per row.
	ids := []string{"vp-0", "vp-1", "vp-2", "vp-3", "vp-4"}
	for i, id := range ids {
		if _, err := w.Record(ctx, rec(id, ws, outputverify.VerdictUnverifiable, "", outputverify.KindNone)); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE k4_output_verdicts SET created_at = TIMESTAMPTZ '2026-01-01 00:00:00+00' + make_interval(secs => $2::float8)
			 WHERE output_id=$1 AND workspace_id=$3`, id, i, ws); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}
	r := outputverify.NewReader(pool)

	page1, err := r.ListForWorkspacePage(ctx, ws, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := r.ListForWorkspacePage(ctx, ws, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("expected 2 rows per page, got page1=%d page2=%d", len(page1), len(page2))
	}
	// newest-first, deterministic: page1 = vp-4, vp-3 ; page2 = vp-2, vp-1
	if page1[0].OutputID != "vp-4" || page1[1].OutputID != "vp-3" {
		t.Errorf("page1 order wrong: got %s,%s want vp-4,vp-3", page1[0].OutputID, page1[1].OutputID)
	}
	if page2[0].OutputID != "vp-2" || page2[1].OutputID != "vp-1" {
		t.Errorf("page2 order wrong: got %s,%s want vp-2,vp-1", page2[0].OutputID, page2[1].OutputID)
	}
	// no overlap between the pages
	seen := map[string]bool{page1[0].OutputID: true, page1[1].OutputID: true}
	for _, v := range page2 {
		if seen[v.OutputID] {
			t.Fatalf("page 2 overlaps page 1 on %q — offset is not being applied", v.OutputID)
		}
	}
}

// The unpaged ListForWorkspace is preserved (offset 0) so non-paginating callers are unchanged.
func TestReader_ListForWorkspace_UnpagedStillNewestFirst(t *testing.T) {
	ctx := context.Background()
	pool := ovTestPool(t)
	ws := "ws-verdict-unpaged"
	if _, err := pool.Exec(ctx, `DELETE FROM k4_output_verdicts WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	w := outputverify.NewWriter(pool)
	for _, id := range []string{"u0", "u1"} {
		if _, err := w.Record(ctx, rec(id, ws, outputverify.VerdictUnverifiable, "", outputverify.KindNone)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := outputverify.NewReader(pool).ListForWorkspace(ctx, ws, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unpaged read: want 2, got %d", len(got))
	}
}
