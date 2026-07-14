// ring_detector.go — Phase-2 anti-gaming: the self-dealing RING detector.
//
// The mint-time linkage guard (sharedFingerprintSQL) DENIES a mint whose
// contributor and requester are DIRECTLY linked. What survives to a held row is
// the ring the pairwise guard misses: link A↔B and B↔C but not A↔C, then the
// mint (contributor C, requester A) passes the direct check while all three are
// one operator. This detector reads the held rows, builds the TRANSITIVE
// identity graph (IdentityGraph), and flags every held mint whose contributor
// and requester land in the same connected component.
//
// READ-ONLY by construction — it holds a detectorDB (Query/QueryRow only, no
// Exec/Begin), the same type-level never-mutate discipline as DetectorReader.
// The clawback is a SEPARATE, deliberately-wired step (the Revoker); a detector
// never burns.
package poolroyalty

import (
	"context"
	"fmt"
	"time"
)

// RingFlag is one held mint the detector judged self-dealing — contributor and
// requester are the same operator (same identity component). Every field is
// evidence: a reviewer (or the auto-adjudicator's audit row) sees exactly why.
type RingFlag struct {
	RequestID            string
	ContributorWorkspace string
	RequesterWorkspace   string
	Amount               int64  // µLENS
	ComponentID          string // the shared operator component both endpoints fall in
	Reason               string // human-readable explanation
}

// RingDetector scans held mints in ONE cross-tenant claim table and flags
// same-operator (ring) self-dealing. The zero/nil detector is inert. `table` is
// a TRUSTED internal constant (pool_royalty_mints / distill_royalty_mints —
// both expose contributor_workspace_id + requester_workspace_id), so the
// fmt.Sprintf interpolation is injection-safe (mirrors sweeper/revoker).
type RingDetector struct {
	db    detectorDB
	table string
}

// NewRingDetector builds a read-only ring detector over an explicit claim table.
func NewRingDetector(db detectorDB, table string) *RingDetector {
	if table == "" {
		table = "pool_royalty_mints"
	}
	return &RingDetector{db: db, table: table}
}

func heldRingSelectSQLFor(table string) string {
	return fmt.Sprintf(`SELECT request_id, contributor_workspace_id, requester_workspace_id, minted_amount
FROM %s
WHERE status = 'held' AND created_at > now() - ($1::bigint * interval '1 microsecond')`, table)
}

// identityEdgesSQL loads every undirected identity edge (two workspaces sharing a
// card fingerprint OR an owner_key). a.workspace_id < b.workspace_id drops
// self-pairs and de-dups direction. This is the whole edge set; the Go union-find
// takes the transitive closure. (Perf note: for very large identity tables this
// should be scoped to keys touching the held-mint workspaces — a follow-on; the
// detector runs on a minute cadence over a bounded held set, and both join keys
// are indexed.)
const identityEdgesSQL = `
SELECT a.workspace_id, b.workspace_id
FROM workspace_card_fingerprints a
JOIN workspace_card_fingerprints b
  ON a.fingerprint_hash = b.fingerprint_hash AND a.workspace_id < b.workspace_id
UNION
SELECT a.workspace_id, b.workspace_id
FROM workspace_owner_links a
JOIN workspace_owner_links b
  ON a.owner_key = b.owner_key AND a.workspace_id < b.workspace_id`

// heldRingRow is one held mint under adjudication.
type heldRingRow struct {
	requestID   string
	contributor string
	requester   string
	amount      int64
}

// DetectSelfDealingRings returns a flag for every held mint (created within
// `window`) whose contributor and requester are the same operator by transitive
// identity closure. Pure read: it never writes, revokes, or finalizes.
func (d *RingDetector) DetectSelfDealingRings(ctx context.Context, window time.Duration) ([]RingFlag, error) {
	if d == nil || d.db == nil {
		return nil, nil
	}
	held, err := d.loadHeld(ctx, window)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, nil
	}
	graph, err := d.loadIdentityGraph(ctx)
	if err != nil {
		return nil, err
	}
	var flags []RingFlag
	for _, h := range held {
		if graph.SameOperator(h.contributor, h.requester) {
			comp := graph.Component(h.contributor)
			flags = append(flags, RingFlag{
				RequestID:            h.requestID,
				ContributorWorkspace: h.contributor,
				RequesterWorkspace:   h.requester,
				Amount:               h.amount,
				ComponentID:          comp,
				Reason: fmt.Sprintf(
					"self-dealing: contributor %s and requester %s are the same operator (identity component %s) — a royalty paid by an operator to itself",
					h.contributor, h.requester, comp),
			})
		}
	}
	return flags, nil
}

// DetectAndPartition scans the held rows ONCE and returns both the EXAMINED set
// (every held request_id it actually read this tick) and the flagged rings. The
// Phase-3 settlement clearer promotes (examined − flagged) to 'cleared'. This is
// what makes fail-closed sound: a row the detector did NOT examine is never in
// `examined`, so it is never cleared → never settles. A detector outage (load
// error) returns err with a NIL examined set, so nothing is cleared on an unknown
// picture — the rows stay held and are retried next tick.
func (d *RingDetector) DetectAndPartition(ctx context.Context, window time.Duration) (examined []string, flags []RingFlag, err error) {
	if d == nil || d.db == nil {
		return nil, nil, nil
	}
	held, err := d.loadHeld(ctx, window)
	if err != nil {
		return nil, nil, err // fail-closed: unknown held set → clear nothing
	}
	examined = make([]string, 0, len(held))
	for _, h := range held {
		examined = append(examined, h.requestID)
	}
	if len(held) == 0 {
		return examined, nil, nil
	}
	graph, err := d.loadIdentityGraph(ctx)
	if err != nil {
		return nil, nil, err // fail-closed: unknown identity graph → clear nothing (nil examined)
	}
	for _, h := range held {
		if graph.SameOperator(h.contributor, h.requester) {
			comp := graph.Component(h.contributor)
			flags = append(flags, RingFlag{
				RequestID:            h.requestID,
				ContributorWorkspace: h.contributor,
				RequesterWorkspace:   h.requester,
				Amount:               h.amount,
				ComponentID:          comp,
				Reason: fmt.Sprintf(
					"self-dealing: contributor %s and requester %s are the same operator (identity component %s) — a royalty paid by an operator to itself",
					h.contributor, h.requester, comp),
			})
		}
	}
	return examined, flags, nil
}

func (d *RingDetector) loadHeld(ctx context.Context, window time.Duration) ([]heldRingRow, error) {
	rows, err := d.db.Query(ctx, heldRingSelectSQLFor(d.table), window.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []heldRingRow
	for rows.Next() {
		var r heldRingRow
		if err := rows.Scan(&r.requestID, &r.contributor, &r.requester, &r.amount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *RingDetector) loadIdentityGraph(ctx context.Context) (*IdentityGraph, error) {
	rows, err := d.db.Query(ctx, identityEdgesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	g := NewIdentityGraph()
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		g.Link(a, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return g, nil
}
