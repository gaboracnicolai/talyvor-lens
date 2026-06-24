// distill_resolver.go — the distill reuse-royalty flag RESOLVER: the distill mirror
// of resolver.go (the cache resolver) over distill_royalty_mints. Same discipline:
//   - CANDIDATES, NEVER VERDICTS — expands a detection flag into the held mints that
//     MATCH ITS PATTERN; legit reuse sits alongside gamed rows (honest over-selection
//     via the ResolutionLabel). It never revokes.
//   - READ-ONLY BY CONSTRUCTION — reuses the resolverDB seam (Query/QueryRow only; no
//     Exec/Begin reachable). TestDistillResolver_NoWriteMethods pins it.
//   - HELD-ONLY — status='held' hard-wired; finalized/revoked rows are out of reach.
//
// TWO resolve types (distill has no similarity — exact-content_hash, so the cache's
// semantic resolver has no analog and is deliberately absent):
//   - volume  → the sock-puppet SWARM: all held mints on one (content_hash, owner)
//     ACROSS requesters (requester is the varying swarm dimension, NOT pinned).
//   - self_dealing → the tight (contributor, requester) pair across all contents — a
//     direct mirror of the cache ResolveSelfDealing, label pair_coarse.
//
// The returned request_ids ARE the distill adjudicate endpoint's revoke_request_ids
// input (both the once-per-relationship SHA256Hex(owner:requester:content_hash) key),
// so this closes detect → resolve → adjudicate to cache parity.
package poolroyalty

import (
	"context"
	"time"
)

// LabelContentSwarm (distill Volume): all held mints on one (content_hash, owner)
// across requesters. Honest over-selection — a genuinely popular doc's organic reuse
// sits in the same set as a sock-puppet swarm; the reviewer decides.
const LabelContentSwarm ResolutionLabel = "content_swarm"

// distillCandidateCols mirrors candidateCols, but distill_royalty_mints has NO
// similarity column → 0::float8 keeps the 9-column scan shape identical so the shared
// Candidate scan (below) works unchanged.
const distillCandidateCols = `request_id, contributor_workspace_id, minted_amount, created_at, finalize_after, status, 0::float8 AS similarity,
       (finalize_after IS NOT NULL AND now() >= finalize_after) AS past_window,
       GREATEST(0, EXTRACT(EPOCH FROM (finalize_after - now())))::float8 AS time_left_secs`

// distillVolumeResolveSQL — the SWARM: held mints on one content from its owner,
// across ALL requesters (requester deliberately NOT in the WHERE — it is the swarm).
const distillVolumeResolveSQL = `SELECT ` + distillCandidateCols + `
FROM distill_royalty_mints
WHERE content_hash = $1
  AND contributor_workspace_id = $2
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($3::bigint * interval '1 microsecond')
ORDER BY created_at`

// distillSelfDealingResolveSQL — direct mirror of selfDealingResolveSQL: the
// (contributor, requester) pair across all contents.
const distillSelfDealingResolveSQL = `SELECT ` + distillCandidateCols + `
FROM distill_royalty_mints
WHERE contributor_workspace_id = $1
  AND requester_workspace_id = $2
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($3::bigint * interval '1 microsecond')
ORDER BY created_at`

// DistillResolver turns distill detection flags into candidate held mints. Reuses the
// read-only resolverDB seam + Candidate/ResolutionResult. The zero/nil resolver is inert.
type DistillResolver struct {
	db resolverDB
}

// NewDistillResolver builds a read-only distill resolver (mirrors NewResolver).
func NewDistillResolver(db resolverDB) *DistillResolver { return &DistillResolver{db: db} }

// ResolveVolume expands a DistillVolumeFlag to the held swarm on (content_hash,
// contributor) across requesters. Label content_swarm (honest over-selection).
func (r *DistillResolver) ResolveVolume(ctx context.Context, f DistillVolumeFlag, window time.Duration) (ResolutionResult, error) {
	if r == nil || r.db == nil {
		return ResolutionResult{}, nil
	}
	cands, err := r.query(ctx, distillVolumeResolveSQL, f.ContentHash, f.ContributorWorkspace, window.Microseconds())
	return ResolutionResult{Candidates: cands, Label: LabelContentSwarm}, err
}

// ResolveSelfDealing expands a DistillSelfDealingFlag to the held (contributor,
// requester) pair across all contents. Label pair_coarse — matches the cache resolver.
func (r *DistillResolver) ResolveSelfDealing(ctx context.Context, f DistillSelfDealingFlag, window time.Duration) (ResolutionResult, error) {
	if r == nil || r.db == nil {
		return ResolutionResult{}, nil
	}
	cands, err := r.query(ctx, distillSelfDealingResolveSQL, f.ContributorWorkspace, f.RequesterWorkspace, window.Microseconds())
	return ResolutionResult{Candidates: cands, Label: LabelPairCoarse}, err
}

// query mirrors Resolver.query — scans the shared Candidate columns (similarity is
// always 0 for distill; the SELECT supplies the constant to keep the scan identical).
func (r *DistillResolver) query(ctx context.Context, sql string, args ...any) ([]Candidate, error) {
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var timeLeftSecs float64
		if err := rows.Scan(&c.RequestID, &c.ContributorWorkspace, &c.MintedAmount, &c.CreatedAt,
			&c.FinalizeAfter, &c.Status, &c.Similarity, &c.PastWindow, &timeLeftSecs); err != nil {
			return nil, err
		}
		c.TimeLeft = time.Duration(timeLeftSecs * float64(time.Second))
		out = append(out, c)
	}
	return out, rows.Err()
}
