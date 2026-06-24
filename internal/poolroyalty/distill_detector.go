// distill_detector.go — distill anti-gaming DETECTORS (PR2): on-demand, read-only
// statistical analysis over distill_royalty_mints that FLAGS gaming patterns for
// HUMAN REVIEW. Mirrors the cache detector (detector.go) and shares its HARD
// INVARIANT: a detector NEVER auto-revokes / slashes / mutates any balance, claim,
// or ledger row — it ONLY returns analysis. Enforced at the TYPE LEVEL via the
// shared read-only `detectorDB` seam (Query/QueryRow only; no Exec/Begin reachable).
// Observe/flag ONLY — blocks nothing. Inert by construction: with minting off there
// are ~no distill_royalty_mints rows, so every detector returns empty.
//
// WHICH cache detectors apply to distill (the data model differs — read this):
//   - BilateralConcentration — DIRECT mirror of the cache self-dealing detector:
//     a (contributor=owner, requester) pair whose mint flow is concentrated through
//     the one counterparty (the wash/collusion signal). Applies unchanged.
//   - VolumeConcentration — ADAPTED. The distill mint is ONCE-PER-RELATIONSHIP
//     (request_id UNIQUE on (owner,requester,content_hash)), so a content_hash earns
//     exactly ONE mint per requester → content_total_mints == distinct_requesters.
//     The cache "few requesters hammering one entry" pattern therefore CANNOT occur.
//     The distill gaming shape is the inverse — a SOCK-PUPPET SWARM: one owner's
//     content reused by MANY distinct requesters in the window (a cluster of puppets
//     each minting once to farm the owner). So distill volume flags HIGH reuse
//     (content_total_mints >= VolumeMinMints), the swarm threshold, not "few
//     requesters". It aggregates ACROSS requesters for one content — catching the
//     cross-requester swarm that the per-pair bilateral detector cannot see.
//   - SimilarityGaming has NO distill analog: it keys on the semantic layer's
//     similarity score and prompt_sha256, columns distill_royalty_mints lacks.
//     Distill OCR reuse is EXACT-content (content_hash = sha256 of the document
//     bytes): a near-duplicate document has a DIFFERENT content_hash and is simply
//     a different relationship the volume detector already covers — there is no
//     similarity distribution to analyze, so this detector is deliberately absent.
//
// STATUS FILTERING (mirrors the cache detector): excludes status='revoked' (a
// clawed-back mint must not re-flag a party) and includes held+final (catch a
// pattern DURING the holdback while a revoke is still possible — a flag over held
// rows is PROVISIONAL; re-run after finalization for the settled picture).
package poolroyalty

import (
	"context"
	"time"
)

// DistillVolumeFlag is one (content_hash, contributor, requester) row with its
// reuse-concentration metrics. Flagged marks a document reused by suspiciously many
// distinct requesters in the window (a sock-puppet swarm). The raw metrics are always
// returned so a reviewer sees the evidence.
type DistillVolumeFlag struct {
	ContentHash                 string
	ContributorWorkspace        string
	RequesterWorkspace          string
	PairContentMints            int
	PairContentMintedUSD        float64
	DistinctRequestersOnContent int
	ContentTotalMints           int
	Flagged                     bool
}

// DistillSelfDealingFlag is one (contributor, requester) pair with bilateral-
// concentration metrics. Flags CONCENTRATION, not common ownership — the data has no
// identity linkage between workspaces, so a flag means "review this pair," never proof
// of collusion (two close legitimate partners look identical to two puppets one owner runs).
type DistillSelfDealingFlag struct {
	ContributorWorkspace  string
	RequesterWorkspace    string
	PairMints             int
	PairMintedUSD         float64
	FracOfContributorFlow float64
	FracOfRequesterFlow   float64
	Flagged               bool
}

// distillVolumeFlagged: a content reused >= VolumeMinMints times in the window
// (== that many distinct requesters, since distill mints once per relationship) is
// the swarm signal. VolumeMaxRequesters (the cache "few requesters" bound) does NOT
// apply to distill — see the file header.
func (t DetectorThresholds) distillVolumeFlagged(contentTotalMints int) bool {
	return t.VolumeMinMints > 0 && contentTotalMints >= t.VolumeMinMints
}

// DistillDetectorReader runs the on-demand distill detectors. The zero/nil reader is
// inert. db is the shared read-only detectorDB (no Exec/Begin reachable — the
// type-level never-auto-act guarantee). Reuses DetectorThresholds (the Volume*/
// Bilateral* fields; the Similarity* fields are unused — distill has no similarity detector).
type DistillDetectorReader struct {
	db detectorDB
	th DetectorThresholds
}

// NewDistillDetectorReader builds a read-only distill detector (mirrors NewDetectorReader).
func NewDistillDetectorReader(db detectorDB, th DetectorThresholds) *DistillDetectorReader {
	return &DistillDetectorReader{db: db, th: th}
}

const distillVolumeSQL = `
WITH windowed AS (
    SELECT content_hash, contributor_workspace_id, requester_workspace_id, minted_amount
    FROM distill_royalty_mints
    WHERE status <> 'revoked'
      AND created_at > now() - ($1::bigint * interval '1 microsecond')
),
content_stats AS (
    SELECT content_hash,
           COUNT(DISTINCT requester_workspace_id) AS distinct_requesters_on_content,
           COUNT(*)                               AS content_total_mints
    FROM windowed GROUP BY content_hash
)
SELECT w.content_hash, w.contributor_workspace_id, w.requester_workspace_id,
       COUNT(*)                          AS pair_content_mints,
       COALESCE(SUM(w.minted_amount), 0) AS pair_content_minted,
       cs.distinct_requesters_on_content,
       cs.content_total_mints
FROM windowed w
JOIN content_stats cs ON cs.content_hash = w.content_hash
GROUP BY w.content_hash, w.contributor_workspace_id, w.requester_workspace_id,
         cs.distinct_requesters_on_content, cs.content_total_mints
ORDER BY cs.content_total_mints DESC, w.content_hash`

// VolumeConcentration flags a content_hash reused by suspiciously many requesters
// (the swarm signal) within the rolling window.
func (r *DistillDetectorReader) VolumeConcentration(ctx context.Context, window time.Duration) ([]DistillVolumeFlag, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, distillVolumeSQL, window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistillVolumeFlag
	for rows.Next() {
		var f DistillVolumeFlag
		if err := rows.Scan(&f.ContentHash, &f.ContributorWorkspace, &f.RequesterWorkspace,
			&f.PairContentMints, &f.PairContentMintedUSD, &f.DistinctRequestersOnContent, &f.ContentTotalMints); err != nil {
			return nil, err
		}
		f.Flagged = r.th.distillVolumeFlagged(f.ContentTotalMints)
		out = append(out, f)
	}
	return out, rows.Err()
}

const distillBilateralSQL = `
WITH pair AS (
    SELECT contributor_workspace_id AS c, requester_workspace_id AS r,
           COUNT(*) AS pair_mints, COALESCE(SUM(minted_amount), 0) AS pair_minted
    FROM distill_royalty_mints
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
// concentrated through the one counterparty (a direct mirror of the cache
// self-dealing detector). Reuses the cache bilateral threshold.
func (r *DistillDetectorReader) BilateralConcentration(ctx context.Context, window time.Duration) ([]DistillSelfDealingFlag, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.Query(ctx, distillBilateralSQL, window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DistillSelfDealingFlag
	for rows.Next() {
		var f DistillSelfDealingFlag
		if err := rows.Scan(&f.ContributorWorkspace, &f.RequesterWorkspace,
			&f.PairMints, &f.PairMintedUSD, &f.FracOfContributorFlow, &f.FracOfRequesterFlow); err != nil {
			return nil, err
		}
		f.Flagged = r.th.bilateralFlagged(f.PairMints, f.FracOfContributorFlow, f.FracOfRequesterFlow)
		out = append(out, f)
	}
	return out, rows.Err()
}
