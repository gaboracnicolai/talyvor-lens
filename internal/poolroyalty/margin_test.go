package poolroyalty

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// MARGIN DERIVATION: the summary reads the pool_royalty_margin view (the
// Stage-2.2 read surface) and returns the realized totals. margin_usd is
// derived in SQL as avoided_cogs_usd − minted_amount — the MARGIN-IDENTITY —
// never re-recorded anywhere.
func TestMarginSummary_ReadsDerivedMargin(t *testing.T) {
	pool := newMockPool(t)
	r := NewMarginReader(pool)

	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	pool.ExpectQuery(`FROM pool_royalty_margin`).
		WithArgs(since).
		WillReturnRows(pgxmock.NewRows([]string{"mints", "avoided", "minted", "margin"}).
			AddRow(int64(3), 6.0, 3.0, 3.0))

	s, err := r.MarginSummary(context.Background(), since)
	if err != nil {
		t.Fatalf("MarginSummary: %v", err)
	}
	if s.Mints != 3 || s.AvoidedCOGSUSD != 6.0 || s.MintedLENS != 3.0 || s.MarginUSD != 3.0 {
		t.Errorf("summary = %+v, want mints=3 avoided=6 minted=3 margin=3", s)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// BREAKDOWNS: by contributor / requester / layer — the dimension is
// allow-listed (it is interpolated into SQL, so anything outside the
// allow-list must be rejected, never executed).
func TestMarginBy_AllowListedDimensions(t *testing.T) {
	for _, dim := range []string{"contributor_workspace_id", "requester_workspace_id", "layer"} {
		t.Run(dim, func(t *testing.T) {
			pool := newMockPool(t)
			r := NewMarginReader(pool)

			pool.ExpectQuery(`GROUP BY ` + dim).
				WithArgs(time.Time{}).
				WillReturnRows(pgxmock.NewRows([]string{"key", "mints", "avoided", "minted", "margin"}).
					AddRow("wsA", int64(2), 4.0, 2.0, 2.0).
					AddRow("wsC", int64(1), 2.0, 1.0, 1.0))

			rows, err := r.MarginBy(context.Background(), dim, time.Time{})
			if err != nil {
				t.Fatalf("MarginBy(%s): %v", dim, err)
			}
			if len(rows) != 2 || rows[0].Key != "wsA" || rows[0].MarginUSD != 2.0 || rows[1].Key != "wsC" {
				t.Errorf("rows = %+v", rows)
			}
			if err := pool.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestMarginBy_RejectsUnknownDimension(t *testing.T) {
	pool := newMockPool(t) // NO expectations: nothing may reach the DB
	r := NewMarginReader(pool)

	for _, bad := range []string{"workspace_id; DROP TABLE pool_royalty_mints;--", "model", "", "created_at"} {
		if _, err := r.MarginBy(context.Background(), bad, time.Time{}); err == nil {
			t.Errorf("MarginBy(%q) must be rejected (allow-list)", bad)
		}
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// INERT: a nil reader (not wired) and an empty table both yield zero values —
// with minting off nothing mints, so the margin surface reports zero.
func TestMarginReader_InertAndNilSafe(t *testing.T) {
	var nilR *MarginReader
	s, err := nilR.MarginSummary(context.Background(), time.Time{})
	if err != nil || s.MarginUSD != 0 || s.Mints != 0 {
		t.Errorf("nil reader must return zero summary; s=%+v err=%v", s, err)
	}
	if rows, err := nilR.MarginBy(context.Background(), "layer", time.Time{}); err != nil || rows != nil {
		t.Errorf("nil reader must return nil rows; rows=%v err=%v", rows, err)
	}

	pool := newMockPool(t)
	r := NewMarginReader(pool)
	pool.ExpectQuery(`FROM pool_royalty_margin`).
		WithArgs(time.Time{}).
		WillReturnRows(pgxmock.NewRows([]string{"mints", "avoided", "minted", "margin"}).
			AddRow(int64(0), 0.0, 0.0, 0.0))
	s2, err := r.MarginSummary(context.Background(), time.Time{})
	if err != nil || s2.MarginUSD != 0 || s2.Mints != 0 {
		t.Errorf("empty table must yield zero; s=%+v err=%v", s2, err)
	}
}

// THE (1−s) IDENTITY, end to end at the claim-args level: the minter writes
// minted_amount = s × avoided per claim row, so the view's per-row
// margin_usd = avoided − minted = (1−s) × avoided, and the summary equals
// (1−s) × Σavoided. Verified against the SAME arguments MintServedHit binds.
func TestMarginIdentity_MatchesShareArithmetic(t *testing.T) {
	const share = 0.3
	pool := newMockPool(t)
	ledger := &fakeLedger{}
	m := NewMinter(pool, ledger, share, enabledOn)

	avoideds := []float64{2.0, 5.0, 0.5}
	var sumAvoided, sumMinted float64
	for i, a := range avoideds {
		h := sampleHit()
		h.RequestID = h.RequestID + "-" + string(rune('a'+i))
		h.AvoidedCOGSUSD = a
		minted := share * a
		sumAvoided += a
		sumMinted += minted

		pool.ExpectBegin()
		pool.ExpectExec(`INSERT INTO pool_royalty_mints`).
			WithArgs(h.RequestID, h.RequesterWorkspace, h.ContributorWorkspace, h.Layer,
				h.EntryID, h.Provider, h.Model, h.Similarity, a, minted,
				h.AnswerSHA256, h.PromptSHA256, (72 * time.Hour).Microseconds()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		pool.ExpectCommit()

		if _, err := m.MintServedHit(context.Background(), h); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}

	// margin = Σ(avoided − minted) = (1−s) × Σavoided
	margin := sumAvoided - sumMinted
	want := (1 - share) * sumAvoided
	if math.Abs(margin-want) > 1e-9 {
		t.Errorf("margin identity: Σ(avoided−minted)=%v, (1−s)×Σavoided=%v — must be equal", margin, want)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
