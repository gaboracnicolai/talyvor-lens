// resolver.go — the pool-mint flag RESOLVER (Stage 2.3b → Stage 3 seam): turns
// a detection FLAG into a set of CANDIDATE held request_ids for HUMAN REVIEW.
//
// CANDIDATES, NEVER VERDICTS. A detection flag points at an entry / a workspace
// pair / a similarity cluster — never at a specific fraudulent mint. The
// resolver expands a flag into the held mints that MATCH ITS PATTERN; it does
// NOT and CANNOT claim those mints are fraudulent. Legitimate organic mints on
// the same tuple are returned alongside gamed ones (the resolver is honest
// about over-selection rather than hiding it — see the ResolutionLabel). A
// human (or the Stage-3 gated trigger) reviews the candidates and decides which
// to feed to the Revoker. The resolver itself never revokes.
//
// READ-ONLY BY CONSTRUCTION: the db handle is the resolverDB interface, which
// exposes ONLY Query/QueryRow — no Exec, no Begin. The resolver literally
// cannot reach a write/revoke primitive (compile-time guarantee, mirrors
// DetectorReader; TestResolver_NoWriteMethods enforces it).
//
// HELD-ONLY: every query carries `status = 'held'`. The resolver surfaces ONLY
// revocable rows. Finalized and revoked mints are deliberately out of reach —
// the honest window-race boundary: a finalized mint is unrevocable and not the
// resolver's concern (consistent with the Option-C poisoning posture). The
// resolver does not chase finalized rows.
package poolroyalty

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ResolutionLabel is the HONEST claim about a resolution's quality — how
// tightly the candidate set maps to the flagged pattern. It is NOT a
// fraud-confidence; it tells the reviewer how much organic traffic the set may
// contain.
type ResolutionLabel string

const (
	// LabelTuplePinned (Volume): pinned to (entry, contributor, requester), but
	// organic mints on that exact tuple are indistinguishable from gamed ones.
	LabelTuplePinned ResolutionLabel = "tuple_pinned"
	// LabelPairCoarse (SelfDealing): the workspace PAIR only, across ALL
	// entries — the coarsest; most likely to include legitimate bilateral rows.
	LabelPairCoarse ResolutionLabel = "pair_coarse"
	// LabelSimilarityNarrowed (Similarity): re-derived by the flag's similarity
	// band — re-selects approximately the engineered cluster.
	LabelSimilarityNarrowed ResolutionLabel = "similarity_narrowed"
	// LabelSimilarityUnnarrowed (Similarity fallback): the flag carried no
	// usable band, so the set is the coarse (contributor, entry) group.
	LabelSimilarityUnnarrowed ResolutionLabel = "similarity_unnarrowed"
)

// Candidate is one HELD mint matching a flag's pattern — a row for human
// review, NOT an adjudicated fraud.
type Candidate struct {
	RequestID            string
	ContributorWorkspace string
	MintedAmount         int64 // µLENS (SEC-2: minted_amount is BIGINT)
	CreatedAt            time.Time
	FinalizeAfter        time.Time
	Status               string  // always "held" (the resolver only surfaces held rows)
	Similarity           float64 // semantic context; 0 for exact-layer rows
	// TimeLeft is how long the mint stays revocable (finalize_after − now),
	// CLAMPED to 0 — never negative.
	TimeLeft time.Duration
	// PastWindow is true when now() >= finalize_after while the row is still
	// 'held' (the sweeper hasn't flipped it yet). Such a row is the MOST
	// time-sensitive candidate — revocable RIGHT NOW, racing the finalize
	// sweeper — not an expired one. The reviewer must see this urgency.
	PastWindow bool
}

// ResolutionResult wraps the candidate set with its honest quality label.
type ResolutionResult struct {
	Candidates []Candidate
	Label      ResolutionLabel
}

// resolverDB is the READ-ONLY db seam — Query/QueryRow only, no Exec/Begin. The
// type-level never-write guarantee. *pgxpool.Pool satisfies it.
type resolverDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Resolver turns flags into candidate held mints. The zero/nil resolver is
// inert. It holds no write surface and calls no revoke path.
type Resolver struct {
	db resolverDB
}

// NewResolver builds a read-only resolver (mirrors NewDetectorReader).
func NewResolver(db resolverDB) *Resolver { return &Resolver{db: db} }

// The shared SELECT shape: every resolver returns the same Candidate columns,
// computing past_window and clamped time_left in SQL so the row reflects the
// DB's clock, not the app's. status='held' is hard-wired into every WHERE.
const candidateCols = `request_id, contributor_workspace_id, minted_amount, created_at, finalize_after, status, similarity,
       (finalize_after IS NOT NULL AND now() >= finalize_after) AS past_window,
       GREATEST(0, EXTRACT(EPOCH FROM (finalize_after - now())))::float8 AS time_left_secs`

const volumeResolveSQL = `SELECT ` + candidateCols + `
FROM pool_royalty_mints
WHERE entry_id = $1
  AND contributor_workspace_id = $2
  AND requester_workspace_id = $3
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($4::bigint * interval '1 microsecond')
ORDER BY created_at`

const selfDealingResolveSQL = `SELECT ` + candidateCols + `
FROM pool_royalty_mints
WHERE contributor_workspace_id = $1
  AND requester_workspace_id = $2
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($3::bigint * interval '1 microsecond')
ORDER BY created_at`

const similarityNarrowedSQL = `SELECT ` + candidateCols + `
FROM pool_royalty_mints
WHERE contributor_workspace_id = $1
  AND entry_id = $2
  AND layer = 'semantic'
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($3::bigint * interval '1 microsecond')
  AND similarity BETWEEN $4 AND $5
ORDER BY created_at`

const similarityUnnarrowedSQL = `SELECT ` + candidateCols + `
FROM pool_royalty_mints
WHERE contributor_workspace_id = $1
  AND entry_id = $2
  AND layer = 'semantic'
  AND status = 'held'
  AND finalize_after IS NOT NULL
  AND created_at > now() - ($3::bigint * interval '1 microsecond')
ORDER BY created_at`

// ResolveVolume expands a VolumeFlag to its held candidates, pinned to
// (entry, contributor, requester). Label tuple_pinned: the set may include
// legitimate organic mints on that same tuple — the resolver does NOT claim to
// distinguish them.
func (r *Resolver) ResolveVolume(ctx context.Context, f VolumeFlag, window time.Duration) (ResolutionResult, error) {
	if r == nil || r.db == nil {
		return ResolutionResult{}, nil
	}
	cands, err := r.query(ctx, volumeResolveSQL, f.EntryID, f.ContributorWorkspace, f.RequesterWorkspace, window.Microseconds())
	return ResolutionResult{Candidates: cands, Label: LabelTuplePinned}, err
}

// ResolveSelfDealing expands a SelfDealingFlag to its held candidates — the
// workspace PAIR only (the flag carries no entry_id), across all entries.
// Label pair_coarse: the coarsest resolution, most likely to sweep in
// legitimate bilateral traffic.
func (r *Resolver) ResolveSelfDealing(ctx context.Context, f SelfDealingFlag, window time.Duration) (ResolutionResult, error) {
	if r == nil || r.db == nil {
		return ResolutionResult{}, nil
	}
	cands, err := r.query(ctx, selfDealingResolveSQL, f.ContributorWorkspace, f.RequesterWorkspace, window.Microseconds())
	return ResolutionResult{Candidates: cands, Label: LabelPairCoarse}, err
}

// ResolveSimilarity expands a SimilarityFlag to its held candidates for the
// (contributor, entry) semantic cluster. When the flag carries a usable
// similarity band (SimMax>0 and SimMax>=SimMin) it RE-APPLIES that band —
// re-selecting approximately the engineered cluster — and labels
// similarity_narrowed; otherwise it falls back to the coarse group and labels
// similarity_unnarrowed.
func (r *Resolver) ResolveSimilarity(ctx context.Context, f SimilarityFlag, window time.Duration) (ResolutionResult, error) {
	if r == nil || r.db == nil {
		return ResolutionResult{}, nil
	}
	if f.SimMax > 0 && f.SimMax >= f.SimMin {
		cands, err := r.query(ctx, similarityNarrowedSQL, f.ContributorWorkspace, f.EntryID, window.Microseconds(), f.SimMin, f.SimMax)
		return ResolutionResult{Candidates: cands, Label: LabelSimilarityNarrowed}, err
	}
	cands, err := r.query(ctx, similarityUnnarrowedSQL, f.ContributorWorkspace, f.EntryID, window.Microseconds())
	return ResolutionResult{Candidates: cands, Label: LabelSimilarityUnnarrowed}, err
}

// query runs a candidate SELECT and scans the shared Candidate columns.
func (r *Resolver) query(ctx context.Context, sql string, args ...any) ([]Candidate, error) {
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
