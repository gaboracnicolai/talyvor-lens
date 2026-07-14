// single_party_detector.go — the P-o-I examined-before-settle detector. The
// Phase-3 SettlementClearer + fail-closed FinalizeSweeper settle ONLY 'cleared'
// rows and clear only what a detector EXAMINED. The RingDetector serves the
// cross-tenant tables (pool/distill); the single-party P-o-I tables
// (routing_prediction_mints, eval_contribution_mints — a workspace earns for its
// own contribution, no contributor↔requester ring) had no examiner, so under
// fail-closed their held rows would strand. This detector closes that: it
// implements the same partitionDetector seam the clearer consumes, flagging
// per-workspace VELOCITY spikes (a farm) while examining every held row.
//
// Read-only (detectorDB is Query/QueryRow-only). The clawback stays the separate
// deliberate Revoker/Adjudicate path; this only refuses to CLEAR a flagged row.
package poolroyalty

import (
	"context"
	"fmt"
	"time"
)

// SinglePartyConcentrationDetector examines held rows of ONE single-party P-o-I
// table and flags per-workspace velocity spikes. Satisfies partitionDetector.
type SinglePartyConcentrationDetector struct {
	db             detectorDB
	table          string // routing_prediction_mints / eval_contribution_mints (trusted internal constant)
	velocityMax    int    // flag a workspace with MORE than this many held mints in velocityWindow
	velocityWindow time.Duration
}

// NewSinglePartyConcentrationDetector builds the detector. velocityMax<=0 disables
// flagging (examines all, flags none — never withholds an honest mint). A
// non-positive velocityWindow defaults to 1h.
func NewSinglePartyConcentrationDetector(db detectorDB, table string, velocityMax int, velocityWindow time.Duration) *SinglePartyConcentrationDetector {
	if velocityWindow <= 0 {
		velocityWindow = time.Hour
	}
	if table == "" {
		table = "routing_prediction_mints"
	}
	return &SinglePartyConcentrationDetector{db: db, table: table, velocityMax: velocityMax, velocityWindow: velocityWindow}
}

func singlePartyHeldSelectSQLFor(table string) string {
	return fmt.Sprintf(`SELECT request_id, contributor_workspace_id, created_at
FROM %s
WHERE status = 'held' AND created_at > now() - ($1::bigint * interval '1 microsecond')`, table)
}

// DetectAndPartition examines every held row in `window` and flags the subset
// belonging to a workspace whose held-mint count in the short velocityWindow
// exceeds velocityMax. Fail-closed: any error returns nil examined so the clearer
// clears nothing.
func (d *SinglePartyConcentrationDetector) DetectAndPartition(ctx context.Context, window time.Duration) (examined []string, flags []RingFlag, err error) {
	if d == nil || d.db == nil {
		return nil, nil, nil
	}
	rows, err := d.db.Query(ctx, singlePartyHeldSelectSQLFor(d.table), window.Microseconds())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type held struct {
		req, ws string
		created time.Time
	}
	var all []held
	for rows.Next() {
		var h held
		if err := rows.Scan(&h.req, &h.ws, &h.created); err != nil {
			return nil, nil, err
		}
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	examined = make([]string, 0, len(all))
	for _, h := range all {
		examined = append(examined, h.req)
	}
	if d.velocityMax <= 0 {
		return examined, nil, nil
	}
	cutoff := time.Now().Add(-d.velocityWindow)
	velWS := map[string]int{}
	for _, h := range all {
		if h.created.After(cutoff) {
			velWS[h.ws]++
		}
	}
	for _, h := range all {
		if velWS[h.ws] > d.velocityMax {
			flags = append(flags, RingFlag{
				RequestID:            h.req,
				ContributorWorkspace: h.ws,
				Reason: fmt.Sprintf("single-party velocity: workspace %s minted >%d %s in %s (farm spike) — withheld from settlement for review",
					h.ws, d.velocityMax, d.table, d.velocityWindow),
			})
		}
	}
	return examined, flags, nil
}
