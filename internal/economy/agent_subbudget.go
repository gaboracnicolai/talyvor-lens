package economy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// agent_subbudget.go — F4-capstone step A: the per-scoped-key LXC sub-budget + the EXACTLY-ONCE agent debit.
// This is the substrate the closed-loop allocator (a later step) sits on. It MINTS NOTHING — it bounds and
// records SPENDING of already-existing LXC (workspace↔Talyvor pool), crediting no ledger and adding no mint
// type. CENTRAL-COUNTERPARTY + CLOSED-LOOP: LXC is debited from the workspace's lxc_balances via the SAME
// internals SpendLXC uses (readLXCBalance/insertLXCLedger/writeLXCBalance) — this file NEVER touches
// LedgerStore.Transfer (LENS P2P) or the marketplace (asserted by agent_subbudget_noloop_test.go).

// ErrSubBudgetExceeded is returned when a debit would push the agent's spent_lxc past its ceiling. The whole
// transaction rolls back — no claim, no debit — so the request_id stays retriable (e.g. after a ceiling raise).
var ErrSubBudgetExceeded = errors.New("economy: agent LXC sub-budget ceiling exceeded")

// DefaultAgentCeilingLXC is the ceiling applied to a scoped key with no explicit ceiling — 50 LXC ($5 at the
// 1 LXC = $0.10 peg), in µLXC (SEC-2). Baked here per the owner's step-A instruction (the capstone ships
// armed with a test ceiling). A funded agent spends up to this; an unfunded one (zero LXC balance) spends nothing.
const DefaultAgentCeilingLXC int64 = 50_000_000

// SetAgentCeiling upserts a scoped key's LXC ceiling in µLXC (preserving spent_lxc). This is how an operator
// sets a per-agent cap other than the default.
func (s *DualTokenStore) SetAgentCeiling(ctx context.Context, scopedKeyID, workspaceID string, ceilingLXC int64) error {
	if s == nil || s.pool == nil {
		return nil
	}
	if scopedKeyID == "" || workspaceID == "" {
		return errors.New("economy: SetAgentCeiling requires scoped_key_id + workspace_id")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0)
		 ON CONFLICT (scoped_key_id) DO UPDATE SET ceiling_lxc = EXCLUDED.ceiling_lxc, updated_at = now()`,
		scopedKeyID, workspaceID, ceilingLXC)
	if err != nil {
		return fmt.Errorf("economy: set agent ceiling: %w", err)
	}
	return nil
}

// AgentDebitMeta is the NON-CONTENT metadata stamped on an agent-allocator lxc_ledger debit row. It
// exists so the ledger is READABLE (per-model spend derivable) and a money row JOINS to its usage row —
// without lxc_ledger, an append-only + immutable financial record (migration 0055), ever becoming a
// content record. It carries EXACTLY three model/id SCALARS and structurally cannot carry prompt text, a
// hash, or an embedding (there is no field for content):
//
//   - RequestedModel: the model the charge was ESTIMATED on. The hold is PRE-SERVE / PRE-ROUTING, so this
//     is the REQUESTED model, NOT necessarily the one that served. Named "requested_model" on the row so a
//     UI cannot imply it was the serving model.
//   - ServedModel: the model that ACTUALLY served, known only post-route at settle time (empty on the
//     pre-serve hold and on a full refund/release, where nothing served). Stamped as "served_model" on the
//     delivered-charge SPEND row so that row is self-describing — "requested X, served Y, charged Z" —
//     without a token_events join. A model name, never content.
//   - RequestID: the token_events request_id, so the money row still joins to its usage row (real token
//     counts) instead of duplicating any of that here.
type AgentDebitMeta struct {
	RequestedModel string
	ServedModel    string
	RequestID      string
}

// toMap renders the scalars as the lxc_ledger metadata document, OMITTING an empty scalar so a row carries
// no empty-string noise. The economy layer owns this shape: a caller supplies only these typed model/id
// strings, never a free-form map, so no content can be injected into a money row.
func (m AgentDebitMeta) toMap() map[string]interface{} {
	out := map[string]interface{}{}
	if m.RequestedModel != "" {
		out["requested_model"] = m.RequestedModel
	}
	if m.ServedModel != "" {
		out["served_model"] = m.ServedModel
	}
	if m.RequestID != "" {
		out["request_id"] = m.RequestID
	}
	return out
}

// SpendLXCForAgent debits lxcAmount from the workspace's LXC balance on behalf of a scoped key (the "agent"),
// EXACTLY ONCE per requestID and only within the agent's remaining sub-budget. All of {claim, ceiling check,
// balance debit, spent bump} happen in ONE transaction — a claim without a debit, or a debit without a
// claim, is the double-spend bug this method exists to prevent. `meta` stamps the debit row with the
// requested model + token_events request_id (non-content; see AgentDebitMeta) so the ledger is readable.
//
// Returns nil on a fresh successful debit AND on an idempotent replay (a requestID already claimed ⇒ nothing
// debited). Returns ErrSubBudgetExceeded (ceiling) or ErrInsufficientLXC (balance) on a rejected debit —
// both roll the whole tx back (no orphan claim, retriable).
func (s *DualTokenStore) SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta AgentDebitMeta) error {
	if lxcAmount <= 0 {
		return errors.New("economy: agent spend amount must be positive")
	}
	if scopedKeyID == "" || workspaceID == "" || requestID == "" {
		return errors.New("economy: agent spend requires scoped_key_id, workspace_id, request_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin agent spend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) EXACTLY-ONCE claim. ON CONFLICT DO NOTHING ⇒ 0 rows means this requestID already succeeded — an
	// idempotent replay: debit NOTHING, return nil. The claim is committed in THIS tx (below) only on success.
	tag, err := tx.Exec(ctx,
		`INSERT INTO lxc_spend_claims (request_id, scoped_key_id, lxc_amount) VALUES ($1, $2, $3)
		 ON CONFLICT (request_id) DO NOTHING`, requestID, scopedKeyID, lxcAmount)
	if err != nil {
		return fmt.Errorf("economy: spend claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // idempotent replay — already debited under this request_id
	}

	// (2) Ensure + LOCK the sub-budget row (FOR UPDATE serializes concurrent debits for this agent).
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0) ON CONFLICT (scoped_key_id) DO NOTHING`,
		scopedKeyID, workspaceID, DefaultAgentCeilingLXC); err != nil {
		return fmt.Errorf("economy: ensure sub-budget: %w", err)
	}
	var ceiling, spent int64 // µLXC
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return fmt.Errorf("economy: read sub-budget: %w", err)
	}

	// (3) CEILING check — reject (rollback ⇒ no orphan claim) if this debit would exceed remaining.
	if ceiling-spent < lxcAmount {
		return ErrSubBudgetExceeded
	}

	// (4) Debit the workspace LXC via the SAME internals SpendLXC uses (no duplication of the balance path).
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID) // FOR UPDATE
	if err != nil {
		return err
	}
	if bal < lxcAmount {
		return ErrInsufficientLXC // rollback ⇒ no orphan claim; retriable after funding
	}
	newBal := bal - lxcAmount // exact integer µLXC
	// Stamp the non-content metadata (requested model + token_events request_id) so the ledger is readable
	// and joins to token_events. meta.toMap() carries ONLY those two scalars — never content (0055 immutable).
	if err := insertLXCLedger(ctx, tx, workspaceID, -lxcAmount, newBal, LXCTypeSpend, description, meta.toMap()); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, wsSpent+lxcAmount); err != nil {
		return err
	}

	// (5) Bump the agent's spent_lxc (monotonic) — atomic with the debit + the claim.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, lxcAmount); err != nil {
		return fmt.Errorf("economy: bump spent: %w", err)
	}

	return tx.Commit(ctx)
}

// ─── RESERVATION lifecycle (billing redesign) ───────────────────────────────
//
// The permanent pre-serve debit (SpendLXCForAgent, above) is replaced by a HOLD → SETTLE/RELEASE pair so
// the customer is billed what was actually DELIVERED, not a pre-serve estimate — while the CEILING stays
// enforced pre-serve against a CONSERVATIVE (output-aware) hold. Every balance move is a NEW lxc_ledger
// row (0055 forbids UPDATE/DELETE on the ledger); only the mutable lifecycle status lives in
// lxc_reservations. Exactly-once: the HOLD is gated by the reservation_id PRIMARY KEY (ON CONFLICT DO
// NOTHING); the resolution is a SELECT ... FOR UPDATE status-CAS (only the first caller to find 'held'
// resolves it, so a settle+release race or a replay is a no-op).

const (
	// LXCTypeReservationHold marks the pre-serve HOLD debit — a bound, NOT a bill. Revenue readers
	// (sum type='spend') MUST exclude it; it nets to zero against its release.
	LXCTypeReservationHold = "reservation_hold"
	// LXCTypeReservationRelease marks the compensating CREDIT that undoes a hold (on settle: refund the
	// unused reservation; on release: refund the whole hold). Append-only-safe (a new row, never an edit).
	LXCTypeReservationRelease = "reservation_release"
)

// ReserveLXCForAgent HOLDS heldLXC of the workspace's LXC against the agent's sub-budget, EXACTLY ONCE per
// reservationID, only within the remaining ceiling. It is the pre-serve gate: the caller BLOCKS the request
// unless this returns nil (mirrors the old SpendLXCForAgent block). heldLXC must be the CONSERVATIVE
// (output-aware) estimate so it is an upper bound on the delivered cost — the ceiling stays airtight. The
// hold is later reconciled by SettleLXCReservation (bill the delivered cost, refund the rest) or refunded in
// full by ReleaseLXCReservation. Returns ErrSubBudgetExceeded / ErrInsufficientLXC on a rejected hold (whole
// tx rolls back — no orphan reservation, retriable); nil on a fresh hold AND on an idempotent replay.
func (s *DualTokenStore) ReserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta AgentDebitMeta) error {
	if heldLXC <= 0 {
		return errors.New("economy: reservation hold amount must be positive")
	}
	if scopedKeyID == "" || workspaceID == "" || reservationID == "" {
		return errors.New("economy: reserve requires scoped_key_id, workspace_id, reservation_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin reserve: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// (1) EXACTLY-ONCE hold claim — reservation_id PK. 0 rows ⇒ this id already holds ⇒ idempotent replay.
	tag, err := tx.Exec(ctx,
		`INSERT INTO lxc_reservations (reservation_id, scoped_key_id, workspace_id, held_ulxc, status, requested_model, request_id)
		 VALUES ($1, $2, $3, $4, 'held', $5, $6) ON CONFLICT (reservation_id) DO NOTHING`,
		reservationID, scopedKeyID, workspaceID, heldLXC, nullIfEmpty(meta.RequestedModel), nullIfEmpty(meta.RequestID))
	if err != nil {
		return fmt.Errorf("economy: reservation claim: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil // idempotent replay — already held under this reservation_id
	}

	// (2) Ceiling — ensure + LOCK the sub-budget (FOR UPDATE serializes concurrent holds for this agent).
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_lxc_subbudgets (scoped_key_id, workspace_id, ceiling_lxc, spent_lxc)
		 VALUES ($1, $2, $3, 0) ON CONFLICT (scoped_key_id) DO NOTHING`,
		scopedKeyID, workspaceID, DefaultAgentCeilingLXC); err != nil {
		return fmt.Errorf("economy: ensure sub-budget: %w", err)
	}
	var ceiling, spent int64
	if err := tx.QueryRow(ctx,
		`SELECT ceiling_lxc, spent_lxc FROM agent_lxc_subbudgets WHERE scoped_key_id = $1 FOR UPDATE`,
		scopedKeyID).Scan(&ceiling, &spent); err != nil {
		return fmt.Errorf("economy: read sub-budget: %w", err)
	}
	if ceiling-spent < heldLXC {
		return ErrSubBudgetExceeded // rollback ⇒ no orphan reservation
	}

	// (3) Debit the workspace LXC for the hold (an immutable lxc_ledger row + balance decrement). The row
	// type is LXCTypeReservationHold — a BOUND, excluded from revenue; the metadata joins to token_events.
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	if bal < heldLXC {
		return ErrInsufficientLXC // rollback ⇒ no orphan reservation; retriable after funding
	}
	newBal := bal - heldLXC
	if err := insertLXCLedger(ctx, tx, workspaceID, -heldLXC, newBal, LXCTypeReservationHold, "reservation hold (pre-serve)", meta.toMap()); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, newBal, minted, wsSpent+heldLXC); err != nil {
		return err
	}
	// (4) Bump spent_lxc by the HELD amount — the ceiling counts the conservative reservation. Settle
	// reclaims the unused difference back into the budget.
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc + $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, heldLXC); err != nil {
		return fmt.Errorf("economy: bump spent (hold): %w", err)
	}
	return tx.Commit(ctx)
}

// SettleLXCReservation reconciles a held reservation to the DELIVERED charge finalLXC: it credits back the
// unused reservation (refund = held − final) and books final as the real bill. final is CLAMPED to [0, held]
// — the conservative hold is an upper bound, and the customer is NEVER charged more than was reserved (belt
// and braces: even a mis-estimated hold cannot over-bill). Two immutable ledger rows in one tx: a
// LXCTypeReservationRelease credit of +held (undo the hold) and a LXCTypeSpend debit of −final (THE bill,
// joined to token_events by request_id) — net balance move +refund. The agent's spent_lxc drops by refund so
// the reserved-but-unspent headroom returns to its budget. Idempotent via the status-CAS: a second settle, or
// a settle racing a release, finds status≠'held' and is a no-op. A settle of an unknown reservation is an
// error (a bug — you cannot bill what you never held).
//
// RETURNS the µLXC ACTUALLY charged (finalLXC after the [0, held] clamp) so the caller can tie a downstream
// action — a cross-tenant royalty mint — to what the consumer REALLY paid. The idempotent no-op and every
// error path return 0 (this call charged nothing new): a royalty funded on a 0 return mints nothing, which
// is the deflationary-safe direction.
func (s *DualTokenStore) SettleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta AgentDebitMeta) (settledULXC int64, err error) {
	if reservationID == "" {
		return 0, errors.New("economy: settle requires reservation_id")
	}
	if finalLXC < 0 {
		finalLXC = 0
	}
	if s == nil || s.pool == nil {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("economy: begin settle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scopedKeyID, workspaceID, status, reqModel, reqID string
	var heldLXC int64
	// requested_model + request_id come from the reservation ROW — the SINGLE source the hold wrote, so the
	// settle's rows stamp exactly what the hold row shows (never a second in-memory copy that could drift).
	err = tx.QueryRow(ctx,
		`SELECT scoped_key_id, workspace_id, held_ulxc, status, COALESCE(requested_model, ''), COALESCE(request_id, '')
		   FROM lxc_reservations WHERE reservation_id = $1 FOR UPDATE`,
		reservationID).Scan(&scopedKeyID, &workspaceID, &heldLXC, &status, &reqModel, &reqID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("economy: settle unknown reservation %q", reservationID)
	}
	if err != nil {
		return 0, fmt.Errorf("economy: read reservation: %w", err)
	}
	if status != "held" {
		return 0, nil // already settled or released — idempotent no-op (charged nothing new)
	}
	if finalLXC > heldLXC {
		finalLXC = heldLXC // never bill above the conservative hold
	}
	refund := heldLXC - finalLXC // ≥ 0

	// Two compensating rows: release the whole hold (+held), then book the delivered charge (−final). Net
	// balance move = +refund. Both are INSERTs — 0055-safe. lifetime_spent nets to +final (was +held at hold).
	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return 0, err
	}
	afterRelease := bal + heldLXC
	// The release (undo the hold) is a refund — nothing served, so requested_model + request_id only.
	if err := insertLXCLedger(ctx, tx, workspaceID, heldLXC, afterRelease, LXCTypeReservationRelease, "reservation settle: release hold",
		AgentDebitMeta{RequestedModel: reqModel, RequestID: reqID}.toMap()); err != nil {
		return 0, err
	}
	afterSpend := afterRelease - finalLXC
	if finalLXC > 0 {
		// The delivered-charge spend row is self-describing: the model the customer REQUESTED (from the row)
		// AND the model that actually SERVED (from the caller, known only post-route), plus the request_id join.
		if err := insertLXCLedger(ctx, tx, workspaceID, -finalLXC, afterSpend, LXCTypeSpend, "reservation settle: delivered charge",
			AgentDebitMeta{RequestedModel: reqModel, ServedModel: meta.ServedModel, RequestID: reqID}.toMap()); err != nil {
			return 0, err
		}
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, afterSpend, minted, wsSpent-refund); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc - $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, refund); err != nil {
		return 0, fmt.Errorf("economy: reclaim spent (settle): %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE lxc_reservations SET status = 'settled', settled_ulxc = $2, resolved_at = now() WHERE reservation_id = $1`,
		reservationID, finalLXC); err != nil {
		return 0, fmt.Errorf("economy: mark settled: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("economy: commit settle: %w", err)
	}
	return finalLXC, nil
}

// ReleaseLXCReservation refunds a held reservation IN FULL (final charge 0): a self-cache hit (no upstream
// call, no contributor ⇒ free), or a serve that never delivered (crash/failure ⇒ the customer must not pay).
// One compensating LXCTypeReservationRelease credit of +held; spent_lxc drops by the whole held. Idempotent
// via the status-CAS. A release of an unknown reservation is a no-op (a stranded-sweeper double-run is safe).
func (s *DualTokenStore) ReleaseLXCReservation(ctx context.Context, reservationID, reason string) error {
	if reservationID == "" {
		return errors.New("economy: release requires reservation_id")
	}
	if s == nil || s.pool == nil {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("economy: begin release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var scopedKeyID, workspaceID, status, reqModel, reqID string
	var heldLXC int64
	// requested_model + request_id from the reservation ROW so the release row (in-band OR stranded-swept —
	// both flow through here) stamps the same model/id the hold recorded. A refund serves nothing → no served_model.
	err = tx.QueryRow(ctx,
		`SELECT scoped_key_id, workspace_id, held_ulxc, status, COALESCE(requested_model, ''), COALESCE(request_id, '')
		   FROM lxc_reservations WHERE reservation_id = $1 FOR UPDATE`,
		reservationID).Scan(&scopedKeyID, &workspaceID, &heldLXC, &status, &reqModel, &reqID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing to release — a double-sweep or a never-held id is a safe no-op
	}
	if err != nil {
		return fmt.Errorf("economy: read reservation: %w", err)
	}
	if status != "held" {
		return nil // already resolved — idempotent
	}

	bal, minted, wsSpent, err := readLXCBalance(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	afterRelease := bal + heldLXC
	desc := "reservation release (full refund)"
	if reason != "" {
		desc = "reservation release: " + reason
	}
	if err := insertLXCLedger(ctx, tx, workspaceID, heldLXC, afterRelease, LXCTypeReservationRelease, desc,
		AgentDebitMeta{RequestedModel: reqModel, RequestID: reqID}.toMap()); err != nil {
		return err
	}
	if err := writeLXCBalance(ctx, tx, workspaceID, afterRelease, minted, wsSpent-heldLXC); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_lxc_subbudgets SET spent_lxc = spent_lxc - $2, updated_at = now() WHERE scoped_key_id = $1`,
		scopedKeyID, heldLXC); err != nil {
		return fmt.Errorf("economy: reclaim spent (release): %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE lxc_reservations SET status = 'released', settled_ulxc = 0, resolved_at = now() WHERE reservation_id = $1`,
		reservationID); err != nil {
		return fmt.Errorf("economy: mark released: %w", err)
	}
	return tx.Commit(ctx)
}

// ReleaseStrandedReservations REFUNDS every hold older than olderThan (a crash between reserve and settle).
// It only ever RELEASES (never settles): a stranded hold's serve outcome is unknown, so the safe money move
// is to give the customer their LXC back. Returns the count released. Idempotent per row via the status-CAS.
func (s *DualTokenStore) ReleaseStrandedReservations(ctx context.Context, olderThan time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := s.pool.Query(ctx,
		`SELECT reservation_id FROM lxc_reservations WHERE status = 'held' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("economy: scan stranded reservations: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := s.ReleaseLXCReservation(ctx, id, "stranded hold swept (crash refund)"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func nullIfEmpty(str string) interface{} {
	if str == "" {
		return nil
	}
	return str
}
