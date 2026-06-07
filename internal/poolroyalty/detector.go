// detector.go — Stage-2.3b anti-gaming DETECTORS: on-demand, read-only
// statistical analysis over pool_royalty_mints that FLAGS gaming patterns for
// HUMAN REVIEW.
//
// HARD INVARIANT — a detector NEVER auto-revokes, auto-slashes, or mutates any
// balance / claim / ledger row. It ONLY returns analysis. The guarantee is
// enforced at the TYPE LEVEL: DetectorReader holds a `detectorDB` whose
// interface exposes only Query/QueryRow — there is no Exec/Begin to reach a
// write, so the type literally cannot mutate (TestDetectorReader_NoWriteMethods
// guards this). Any actual revoke remains the deliberate operator action via
// the 2.3a RevokeHeldTx within the holdback window; detection INFORMS that
// decision, it never triggers it.
//
// The cap (2.3b primitive #1) BOUNDS exposure; these detectors FLAG patterns.
// They are pure reads: no new column, no ledger write, no lock, no spend-reader
// surface, nothing of the stake/HA machinery. Inert by construction — with
// minting off there are ~no rows, so every detector returns empty.
//
// STATUS FILTERING (correctness): every query excludes status='revoked' (an
// already-clawed-back mint must not inflate or re-flag a party) and INCLUDES
// status IN ('held','final') deliberately — detection's value is catching a
// pattern DURING the holdback window, while RevokeHeldTx is still possible. A
// flag computed over held rows is therefore PROVISIONAL (those rows may yet
// finalize or be revoked); re-running after finalization gives the settled
// picture.
//
// Thresholds tune only the `Flagged` boolean — the raw metrics are always
// returned so a reviewer sees the full evidence.
package poolroyalty

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// DetectorThresholds tunes the Flagged boolean of each detector. Built by the
// caller from config (kept out of config's import graph — config holds the raw
// fields, the caller maps them here).
type DetectorThresholds struct {
	VolumeMinMints      int     // entry total mints at/above which volume concentration can flag
	VolumeMaxRequesters int     // ...AND distinct requesters at/below which it flags
	BilateralMinFrac    float64 // both sides' counterparty-flow fraction at/above which a pair flags
	BilateralMinMints   int     // ...AND pair mints at/above which it flags
	SimilarityMinSample int     // min hits per (contributor, entry) for the similarity test to apply
	SimilarityMaxStddev float64 // similarity stddev at/below which a cluster is "tight" (engineered)
}

func (t DetectorThresholds) volumeFlagged(entryTotalMints, distinctRequesters int) bool {
	return entryTotalMints >= t.VolumeMinMints && distinctRequesters <= t.VolumeMaxRequesters
}

func (t DetectorThresholds) bilateralFlagged(pairMints int, fracContributor, fracRequester float64) bool {
	return pairMints >= t.BilateralMinMints &&
		fracContributor >= t.BilateralMinFrac && fracRequester >= t.BilateralMinFrac
}

func (t DetectorThresholds) similarityFlagged(hits, distinctPrompts int, stddev float64) bool {
	// Engineered near-duplicates: a tight similarity cluster AND a MAJORITY of
	// distinct prompts (organic re-asks repeat one prompt_sha256 → low
	// distinct; engineered dupes are many different prompts at one similarity →
	// high distinct). The HAVING clause already enforces the sample floor; the
	// hits>=MinSample check here is belt-and-braces.
	return hits >= t.SimilarityMinSample &&
		stddev <= t.SimilarityMaxStddev &&
		distinctPrompts*2 >= hits
}

// VolumeFlag is one (entry, contributor, requester) row with its concentration
// metrics. Flagged marks entries with high total mints but few distinct
// requesters; the raw metrics are always present so the reviewer sees the
// evidence behind the flag.
type VolumeFlag struct {
	EntryID                   string
	ContributorWorkspace      string
	RequesterWorkspace        string
	PairEntryMints            int
	PairEntryMintedUSD        float64
	DistinctRequestersOnEntry int
	EntryTotalMints           int
	Flagged                   bool
}

// SelfDealingFlag is one (contributor, requester) pair with bilateral-
// concentration metrics. NOTE: this flags CONCENTRATION, not common
// ownership — the data has no identity linkage between workspaces, so a
// flag means "review this pair," never proof of collusion (two close
// legitimate partners look identical to two workspaces with one owner).
type SelfDealingFlag struct {
	ContributorWorkspace  string
	RequesterWorkspace    string
	PairMints             int
	PairMintedUSD         float64
	FracOfContributorFlow float64
	FracOfRequesterFlow   float64
	Flagged               bool
}

// SimilarityFlag is one (contributor, entry) semantic cluster with its
// similarity-distribution metrics.
type SimilarityFlag struct {
	ContributorWorkspace string
	EntryID              string
	Hits                 int
	DistinctPrompts      int
	SimMean              float64
	SimStddev            float64
	SimMin               float64
	SimMax               float64
	Flagged              bool
}

// detectorDB is the READ-ONLY db seam — deliberately only Query/QueryRow, no
// Exec/Begin. This is the type-level never-auto-act guarantee: a DetectorReader
// cannot reach a write primitive. *pgxpool.Pool satisfies it.
type detectorDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DetectorReader runs the on-demand detectors. The zero/nil reader is inert.
// Its `db` field is the read-only detectorDB interface — the type-level
// never-auto-act guarantee (no Exec/Begin reachable).
type DetectorReader struct {
	db detectorDB
	th DetectorThresholds
}

// NewDetectorReader builds a read-only detector over db with the given
// thresholds (mirrors NewMarginReader; takes thresholds by value so config
// stays out of this package's import graph).
func NewDetectorReader(db detectorDB, th DetectorThresholds) *DetectorReader {
	return &DetectorReader{db: db, th: th}
}

const volumeSQL = `
WITH windowed AS (
    SELECT entry_id, contributor_workspace_id, requester_workspace_id, minted_amount
    FROM pool_royalty_mints
    WHERE status <> 'revoked'
      AND created_at > now() - ($1::bigint * interval '1 microsecond')
),
entry_stats AS (
    SELECT entry_id,
           COUNT(DISTINCT requester_workspace_id) AS distinct_requesters_on_entry,
           COUNT(*)                               AS entry_total_mints
    FROM windowed GROUP BY entry_id
)
SELECT w.entry_id, w.contributor_workspace_id, w.requester_workspace_id,
       COUNT(*)                          AS pair_entry_mints,
       COALESCE(SUM(w.minted_amount), 0) AS pair_entry_minted,
       es.distinct_requesters_on_entry,
       es.entry_total_mints
FROM windowed w
JOIN entry_stats es ON es.entry_id = w.entry_id
GROUP BY w.entry_id, w.contributor_workspace_id, w.requester_workspace_id,
         es.distinct_requesters_on_entry, es.entry_total_mints
ORDER BY es.entry_total_mints DESC, w.entry_id`

// VolumeConcentration flags entries hammered by few requesters within the
// rolling window.
func (r *DetectorReader) VolumeConcentration(ctx context.Context, window time.Duration) ([]VolumeFlag, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, volumeSQL, window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VolumeFlag
	for rows.Next() {
		var f VolumeFlag
		if err := rows.Scan(&f.EntryID, &f.ContributorWorkspace, &f.RequesterWorkspace,
			&f.PairEntryMints, &f.PairEntryMintedUSD, &f.DistinctRequestersOnEntry, &f.EntryTotalMints); err != nil {
			return nil, err
		}
		f.Flagged = r.th.volumeFlagged(f.EntryTotalMints, f.DistinctRequestersOnEntry)
		out = append(out, f)
	}
	return out, rows.Err()
}

const bilateralSQL = `
WITH pair AS (
    SELECT contributor_workspace_id AS c, requester_workspace_id AS r,
           COUNT(*) AS pair_mints, COALESCE(SUM(minted_amount), 0) AS pair_minted
    FROM pool_royalty_mints
    WHERE status <> 'revoked'
      AND created_at > now() - ($1::bigint * interval '1 microsecond')
    GROUP BY 1, 2
),
contrib_total AS (SELECT c, SUM(pair_mints) AS total FROM pair GROUP BY c),
req_total     AS (SELECT r, SUM(pair_mints) AS total FROM pair GROUP BY r)
SELECT p.c, p.r, p.pair_mints, p.pair_minted,
       p.pair_mints::float / ct.total AS frac_of_contributor_flow,
       p.pair_mints::float / rt.total AS frac_of_requester_flow
FROM pair p
JOIN contrib_total ct ON ct.c = p.c
JOIN req_total     rt ON rt.r = p.r
ORDER BY p.pair_mints DESC, p.c, p.r`

// BilateralConcentration flags (contributor, requester) pairs whose flow is
// concentrated through the one counterparty.
func (r *DetectorReader) BilateralConcentration(ctx context.Context, window time.Duration) ([]SelfDealingFlag, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, bilateralSQL, window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SelfDealingFlag
	for rows.Next() {
		var f SelfDealingFlag
		if err := rows.Scan(&f.ContributorWorkspace, &f.RequesterWorkspace,
			&f.PairMints, &f.PairMintedUSD, &f.FracOfContributorFlow, &f.FracOfRequesterFlow); err != nil {
			return nil, err
		}
		f.Flagged = r.th.bilateralFlagged(f.PairMints, f.FracOfContributorFlow, f.FracOfRequesterFlow)
		out = append(out, f)
	}
	return out, rows.Err()
}

const similaritySQL = `
SELECT contributor_workspace_id, entry_id,
       COUNT(*)                         AS hits,
       COUNT(DISTINCT prompt_sha256)    AS distinct_prompts,
       COALESCE(AVG(similarity), 0)     AS sim_mean,
       COALESCE(STDDEV_POP(similarity), 0) AS sim_stddev,
       COALESCE(MIN(similarity), 0)     AS sim_min,
       COALESCE(MAX(similarity), 0)     AS sim_max
FROM pool_royalty_mints
WHERE layer = 'semantic' AND status <> 'revoked'
  AND created_at > now() - ($1::bigint * interval '1 microsecond')
GROUP BY contributor_workspace_id, entry_id
HAVING COUNT(*) >= $2
ORDER BY sim_stddev ASC, hits DESC`

// SimilarityGaming flags semantic (contributor, entry) clusters of many
// distinct prompts landing at tight near-identical similarity.
func (r *DetectorReader) SimilarityGaming(ctx context.Context, window time.Duration) ([]SimilarityFlag, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, similaritySQL, window.Microseconds(), r.th.SimilarityMinSample)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SimilarityFlag
	for rows.Next() {
		var f SimilarityFlag
		if err := rows.Scan(&f.ContributorWorkspace, &f.EntryID, &f.Hits, &f.DistinctPrompts,
			&f.SimMean, &f.SimStddev, &f.SimMin, &f.SimMax); err != nil {
			return nil, err
		}
		f.Flagged = r.th.similarityFlagged(f.Hits, f.DistinctPrompts, f.SimStddev)
		out = append(out, f)
	}
	return out, rows.Err()
}
