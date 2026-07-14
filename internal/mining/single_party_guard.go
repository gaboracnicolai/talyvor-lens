// single_party_guard.go — Phase-4a Item 3: the SINGLE-PARTY examined-before-settle
// guard over traffic_mint_holds (the pattern held mint, and any future
// single-party traffic mint). Phase-2/3 gave the CROSS-TENANT tables
// (pool/distill) a RING detector + settlement clearer + fail-closed finalize; the
// single-party tables had no examination and settled on the timer. This closes
// that gap uniformly: a held row the detector never examined is never cleared →
// the fail-closed TrafficMintSweeper never settles it.
//
// Single-party mints have no contributor↔requester edge — no ring. The abuse
// signal is per-workspace VELOCITY: a workspace whose held-mint count in the
// detection window spikes past a threshold (a farm) is flagged; its mints are
// withheld from settlement (stay held for review) while honest low-velocity
// workspaces clear and settle. DEFAULT OFF; FAIL-CLOSED on a detector error.
package mining

import (
	"context"
	"log/slog"
	"time"
)

// (TrafficHoldKey — the composite {RequestID, WorkspaceID, MintType} identity — is
// defined in traffic_revoker.go and reused here.)

// SinglePartyConcentrationDetector examines held traffic_mint_holds rows of ONE
// mint_type and flags per-workspace velocity spikes. Read-only. The zero/nil
// detector is inert.
type SinglePartyConcentrationDetector struct {
	db             pgxDB
	mintType       string
	velocityMax    int           // flag a workspace with MORE than this many held mints in velocityWindow
	velocityWindow time.Duration // the SHORT spike window the velocity is measured over
}

// NewSinglePartyConcentrationDetector builds the detector over a mint_type (a
// trusted internal constant). velocityMax<=0 disables flagging (examines all,
// flags none) — a safe default that never withholds an honest mint. A
// non-positive velocityWindow defaults to 1h.
func NewSinglePartyConcentrationDetector(db pgxDB, mintType string, velocityMax int, velocityWindow time.Duration) *SinglePartyConcentrationDetector {
	if velocityWindow <= 0 {
		velocityWindow = time.Hour
	}
	return &SinglePartyConcentrationDetector{db: db, mintType: mintType, velocityMax: velocityMax, velocityWindow: velocityWindow}
}

const singlePartyHeldSelectSQL = `SELECT request_id, workspace_id, created_at
FROM traffic_mint_holds
WHERE mint_type = $1 AND status = 'held' AND created_at > now() - ($2::bigint * interval '1 microsecond')`

// DetectAndPartition examines every held row in `window` and flags the subset
// belonging to a workspace whose held-mint count IN THE SHORT velocityWindow
// exceeds velocityMax (a spike). Examining over the full (long) window while
// measuring velocity over a short sub-window lets a spike be caught while every
// held row still gets examined before it is due. Fail-closed: any error returns
// nil examined so the clearer clears nothing.
func (d *SinglePartyConcentrationDetector) DetectAndPartition(ctx context.Context, window time.Duration) (examined, flagged []TrafficHoldKey, err error) {
	if d == nil || d.db == nil {
		return nil, nil, nil
	}
	rows, err := d.db.Query(ctx, singlePartyHeldSelectSQL, d.mintType, window.Microseconds())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type heldRow struct {
		key       TrafficHoldKey
		createdAt time.Time
	}
	var held []heldRow
	for rows.Next() {
		var r heldRow
		r.key.MintType = d.mintType
		if err := rows.Scan(&r.key.RequestID, &r.key.WorkspaceID, &r.createdAt); err != nil {
			return nil, nil, err
		}
		held = append(held, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	examined = make([]TrafficHoldKey, 0, len(held))
	for _, r := range held {
		examined = append(examined, r.key)
	}
	if d.velocityMax <= 0 {
		return examined, nil, nil
	}
	// Velocity = per-workspace held-mint count within the short velocityWindow from now.
	cutoff := time.Now().Add(-d.velocityWindow)
	velWS := map[string]int{}
	for _, r := range held {
		if r.createdAt.After(cutoff) {
			velWS[r.key.WorkspaceID]++
		}
	}
	for _, r := range held {
		if velWS[r.key.WorkspaceID] > d.velocityMax {
			flagged = append(flagged, r.key)
		}
	}
	return examined, flagged, nil
}

// ─── the traffic settlement clearer (single-party examined-before-settle) ──────

const trafficClearCASSQL = `UPDATE traffic_mint_holds SET status = 'cleared'
WHERE mint_type = $1 AND status = 'held' AND finalize_after < now()
  AND (request_id, workspace_id) IN (SELECT r, w FROM unnest($2::text[], $3::text[]) AS t(r, w))`

// TrafficSettlementClearer promotes examined-clean-AND-due held traffic rows to
// 'cleared' so the fail-closed TrafficMintSweeper can settle them. Mirrors
// poolroyalty.SettlementClearer for the composite-keyed traffic table. DEFAULT
// OFF; FAIL-CLOSED on a detector error (clears nothing, retried next tick).
type TrafficSettlementClearer struct {
	detector *SinglePartyConcentrationDetector
	db       pgxDB
	enabled  func() bool
	window   time.Duration
	health   *DetectorHealth // Phase-4a Item 4: heartbeat, so a stall is visible
}

// SetHealth attaches a detector-health tracker; the clearer marks it on each
// successful run so a stall (fail-closed strands mints) is observable.
func (c *TrafficSettlementClearer) SetHealth(h *DetectorHealth) {
	if c != nil {
		c.health = h
	}
}

func NewTrafficSettlementClearer(detector *SinglePartyConcentrationDetector, db pgxDB, enabled func() bool, window time.Duration) *TrafficSettlementClearer {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &TrafficSettlementClearer{detector: detector, db: db, enabled: enabled, window: window}
}

// RunOnce promotes examined-clean-and-due held rows to 'cleared', returning the
// count cleared. Inert when disabled; fail-closed on a detector error.
func (c *TrafficSettlementClearer) RunOnce(ctx context.Context) (int, error) {
	if c == nil || c.detector == nil || c.db == nil || c.enabled == nil || !c.enabled() {
		return 0, nil // DEFAULT OFF
	}
	examined, flagged, err := c.detector.DetectAndPartition(ctx, c.window)
	if err != nil {
		return 0, err // FAIL-CLOSED (do NOT mark healthy on an error — a failing detector is a stall)
	}
	c.health.MarkRun() // the detector ran cleanly this tick — heartbeat (nil-safe)
	if len(examined) == 0 {
		return 0, nil
	}
	bad := make(map[TrafficHoldKey]struct{}, len(flagged))
	for _, k := range flagged {
		bad[k] = struct{}{}
	}
	reqs := make([]string, 0, len(examined))
	wss := make([]string, 0, len(examined))
	for _, k := range examined {
		if _, isBad := bad[k]; isBad {
			continue // examined − flagged
		}
		reqs = append(reqs, k.RequestID)
		wss = append(wss, k.WorkspaceID)
	}
	if len(reqs) == 0 {
		return 0, nil
	}
	tag, err := c.db.Exec(ctx, trafficClearCASSQL, c.detector.mintType, reqs, wss)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// StartScheduler ticks RunOnce until ctx ends — mirrors the finalize/settlement
// sweepers. Leader-elected by the caller. Inert while disabled; fail-closed on error.
func (c *TrafficSettlementClearer) StartScheduler(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.RunOnce(ctx); err != nil {
				slog.Warn("traffic settlement clearer: sweep failed (fail-closed; nothing cleared this tick)",
					slog.String("mint_type", c.detector.mintType), slog.String("error", err.Error()))
			}
		}
	}
}

