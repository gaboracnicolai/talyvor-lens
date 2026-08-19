package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/localrouter"
)

// agent_allocator.go — F4-capstone step C.1: the live bounded-allocation glue. For an AGENT request (an
// API-key-authed request with LXCAgentAllocationEnabled on), the pre-serve seam debits the input-only LXC
// estimate against the per-scoped-key sub-budget via SpendLXCForAgent — BEFORE node selection and BEFORE the
// upstream call — so a blocked agent physically cannot reach the serve path or exceed its ceiling.
//
// CLOSED-LOOP + CENTRAL-COUNTERPARTY: the only value movement is the SpendLXCForAgent debit (workspace↔Talyvor
// pool); this file adds no Transfer/marketplace/refund path (asserted by agent_allocator_guard_test.go).
//
// SERVER-DERIVED DEBIT KEY: the request_id passed to SpendLXCForAgent is derived from a process-start random
// salt + the apiKeyID + a per-request crypto/rand nonce — NEVER the client X-Talyvor-Request-ID header. So a
// client replaying its header gets a fresh key and is charged again (cannot dodge); the claim table's
// exactly-once still protects a server-side retry that reuses the SAME derived key.

// agentSpender is the minimal debit surface (economy.DualTokenStore.SpendLXCForAgent satisfies it). An
// interface so the proxy doesn't hard-depend on economy internals.
type agentSpender interface {
	SpendLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, requestID string, lxcAmount int64, description string, meta economy.AgentDebitMeta) error
	// Reservation lifecycle (billing redesign) — satisfied by *economy.DualTokenStore. Settle returns the
	// µLXC ACTUALLY charged (clamped to the hold) so the caller can fund a royalty on the real payment.
	ReserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta economy.AgentDebitMeta) error
	// Returns the settled charge AND the CASH-BACKED portion of it. Two figures because they answer
	// different questions: the charge is what the customer paid, the cash-backed portion is what may
	// fund a royalty. Collapsing them would either under-bill the customer or over-mint the pool.
	SettleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta economy.AgentDebitMeta) (settled, cashBacked int64, err error)
	ReleaseLXCReservation(ctx context.Context, reservationID, reason string) error
}

// SetAgentSpender wires the agent-allocation debit + its enable flag, and mints the process-start salt used
// to derive debit keys. nil-safe: agentAllocationBlocks is inert until this is called.
func (p *Proxy) SetAgentSpender(spender agentSpender, enabled func() bool) {
	p.agentSpender = spender
	p.agentAllocEnabled = enabled
	if p.agentDebitSalt == nil {
		salt := make([]byte, 32)
		if _, err := rand.Read(salt); err != nil {
			// crypto/rand failure ⇒ leave salt nil so the gate fails CLOSED (deriveAgentDebitKey errors).
			slog.Error("economy: agent debit salt init failed (agent allocation fails closed)", slog.String("err", err.Error()))
			return
		}
		p.agentDebitSalt = salt
	}
}

// deriveAgentDebitKey = hex(SHA256(salt ‖ apiKeyID ‖ nonce)), nonce = 32 crypto/rand bytes. Server-derived +
// per-request-unique: the client header never participates, so a header replay cannot reuse a key.
func deriveAgentDebitKey(salt []byte, apiKeyID string) (string, error) {
	if len(salt) == 0 {
		return "", errors.New("proxy: agent debit salt not initialized")
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(apiKeyID))
	h.Write(nonce)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// agentKeyIDFromContext returns the scoped API-key ID for an API-key-authed request, else "" (JWT/admin/anon
// carry no APIKeyID — C.0 guarantees this, so they structurally cannot enter the agent path).
func agentKeyIDFromContext(ctx context.Context) string {
	if actx := auth.GetAuthContext(ctx); actx != nil {
		return actx.APIKeyID
	}
	return ""
}

// agentStrategy picks the routing strategy: price-aware for an active agent request (B's clamp bounds Lens's
// node cost), else the default least-loaded — non-agent traffic is unchanged.
func (p *Proxy) agentStrategy(apiKeyID string) localrouter.RoutingStrategy {
	if apiKeyID != "" && p.agentAllocEnabled != nil && p.agentAllocEnabled() && p.agentSpender != nil {
		return localrouter.StrategyPriceAware
	}
	return localrouter.StrategyLeastLoaded
}

// agentAllocationBlocks performs the PRE-SERVE agent debit and reports whether the request must be BLOCKED
// (402). Inert (returns false, no debit) for a non-agent request, flag-off, unwired spender, or a
// zero/unknown estimate. Otherwise it debits the input-only LXC estimate against the sub-budget via
// SpendLXCForAgent, keyed on a server-derived request id, and BLOCKS unless the debit succeeded — so the
// serve path is entered IFF a debit was booked (airtight ceiling). Fail-CLOSED: any debit error blocks (a
// bounded agent must not serve on an unverifiable budget).
// requestID is the token_events request id (the handler's per-request id), stamped onto the debit row's
// metadata so the money row joins to its usage row. `model` is the REQUESTED model — this debit precedes
// routing, so it is what the charge was estimated on, not necessarily the model that served.
func (p *Proxy) agentAllocationBlocks(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string) bool {
	if apiKeyID == "" || p.agentSpender == nil || p.agentAllocEnabled == nil || !p.agentAllocEnabled() {
		return false // non-agent / inert → no debit, no block (today's behavior)
	}
	estLXC := lxcEstimate(model, prompt)
	if estLXC <= 0 {
		// An UNKNOWN MODEL no longer lands here — lxcEstimate prices it on the derived floor — which is
		// the whole point: this branch used to serve unpriced traffic with no debit at all, and it is
		// the path taken whenever reservations are off.
		//
		// ⚠ IT IS NOT "EMPTY PROMPT ONLY", WHICH IS WHAT THIS SAID. lxcEstimate converts bytes to
		// tokens with len(prompt)/4, which FLOORS, so ANY prompt under 4 bytes is zero tokens, prices
		// at zero on every model, and takes this branch. MEASURED against a FULLY EXHAUSTED sub-budget
		// on real Postgres (TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt): a
		// 77-byte prompt is BLOCKED and "ok?" / "go" / "y" are SERVED, booking no lxc_spend_claims row
		// at all. So the "airtight ceiling" above — "the serve path is entered IFF a debit was booked"
		// — holds for every prompt except the short ones, and an agent at its ceiling keeps drawing
		// completions whose OUTPUT this input-only estimate never bounded.
		//
		// ⚠ NOT REPAIRED HERE BECAUSE EVERY REPAIR MOVES A NUMBER: ceil-ing the token count re-prices
		// every prompt whose length is not a multiple of 4; a 1 µLXC floor invents a charge;
		// blocking short prompts refuses traffic that is served today. That is a pricing/product
		// decision. The boundary is pinned in lxc_estimate_short_prompt_test.go so it cannot widen
		// silently, and the same free path exists in lxc_gate.go#lxcGateBlocks.
		return false
	}
	debitKey, err := deriveAgentDebitKey(p.agentDebitSalt, apiKeyID)
	if err != nil {
		slog.Error("economy: agent debit key derivation failed (failing closed)", slog.String("err", err.Error()))
		return true // fail closed — cannot mint a safe key ⇒ do not serve
	}
	// The debit row carries the REQUESTED model + token_events request_id (non-content — AgentDebitMeta),
	// so the ledger is readable and joins to token_events. NEVER prompt text/hash/embedding (0055 immutable).
	err = p.agentSpender.SpendLXCForAgent(ctx, apiKeyID, wsID, debitKey, estLXC, "proof-of-agent-allocation: pre-serve estimate debit",
		economy.AgentDebitMeta{RequestedModel: model, RequestID: requestID})
	if err == nil {
		return false // debited ⇒ allow (serve)
	}
	if !errors.Is(err, economy.ErrSubBudgetExceeded) && !errors.Is(err, economy.ErrInsufficientLXC) {
		// Unexpected (e.g. transient DB) error — fail CLOSED to keep the ceiling airtight.
		slog.Warn("economy: agent debit failed (failing closed)", slog.String("agent", apiKeyID), slog.String("err", err.Error()))
	}
	return true // ErrSubBudgetExceeded / ErrInsufficientLXC / any error ⇒ block (402)
}

// ─── Reservation seam (billing redesign) ────────────────────────────────────
//
// When LXCReservationEnabled, the pre-serve seam RESERVES a conservative (output-aware) hold instead of
// permanently debiting an estimate; the post-serve seam SETTLES the DELIVERED cost or RELEASES a cache hit
// (free). The reservation id is the same server-derived debit key; it rides the request context from the
// hold to the settle/release so no serve-path plumbing threads it by hand.

type reservationCtxKey struct{}

// reservationHandle is what the pre-serve hold parks on the context for the post-serve seam to resolve.
type reservationHandle struct {
	reservationID string
	requestID     string // token_events join, stamped on the settle's delivered-charge row
}

func withReservation(ctx context.Context, h reservationHandle) context.Context {
	return context.WithValue(ctx, reservationCtxKey{}, h)
}

func reservationFrom(ctx context.Context) (reservationHandle, bool) {
	h, ok := ctx.Value(reservationCtxKey{}).(reservationHandle)
	return h, ok
}

// reservationActive reports whether the reservation path is on AND wired.
func (p *Proxy) reservationActive() bool {
	return p.reservationEnabled != nil && p.reservationEnabled() && p.agentSpender != nil && p.agentAllocEnabled != nil && p.agentAllocEnabled()
}

// agentReserveBlocks performs the PRE-SERVE HOLD and reports whether the request must be BLOCKED (402). On a
// successful hold it returns a context carrying the reservation handle (for settle/release) and false. Inert
// (returns ctx, false — no hold) for a non-agent request or a zero estimate. Fail-CLOSED: any hold error
// blocks (a bounded agent must not serve on an unverifiable budget). maxOut is the caller-BOUNDED output
// allowance (explicit max_tokens else the configured cap) so the hold is a conservative upper bound.
func (p *Proxy) agentReserveBlocks(ctx context.Context, apiKeyID, wsID, model, prompt, requestID string, maxOut int) (context.Context, bool) {
	if apiKeyID == "" || p.agentSpender == nil {
		return ctx, false
	}
	heldLXC := reserveEstimateLXC(model, prompt, maxOut)
	if heldLXC <= 0 {
		return ctx, false // unknown model / empty → allow, like the old estimate path
	}
	reservationID, err := deriveAgentDebitKey(p.agentDebitSalt, apiKeyID)
	if err != nil {
		slog.Error("economy: reservation key derivation failed (failing closed)", slog.String("err", err.Error()))
		return ctx, true
	}
	err = p.agentSpender.ReserveLXCForAgent(ctx, apiKeyID, wsID, reservationID, heldLXC,
		economy.AgentDebitMeta{RequestedModel: model, RequestID: requestID})
	if err != nil {
		if !errors.Is(err, economy.ErrSubBudgetExceeded) && !errors.Is(err, economy.ErrInsufficientLXC) {
			slog.Warn("economy: reservation hold failed (failing closed)", slog.String("agent", apiKeyID), slog.String("err", err.Error()))
		}
		return ctx, true // ceiling / insufficient / any error ⇒ block (402)
	}
	return withReservation(ctx, reservationHandle{reservationID: reservationID, requestID: requestID}), false
}

// settleReservation SETTLES the held reservation (if any) to the DELIVERED cost — called at the post-serve
// seam so the hold is released PROMPTLY, not on a timer (the sweeper is for crashes only). deliveredUSD is
// converted to µLXC (ceil); the primitive clamps to [0, held] so a mis-estimate cannot over-bill.
//
// RETURNS the USD the consumer was ACTUALLY charged for this request (0 when there is NO reservation — a
// non-agent/plain-key request, or the reservation path off — and 0 on a settle error). The cross-tenant
// royalty seam reads this: a royalty is funded ONLY by what the consumer really paid, so a 0 here mints 0.
// servedModel is the model that ACTUALLY served this request (post-route) — stamped on the delivered-charge
// spend row so the money row reads "requested X, served Y, charged Z". requested_model + request_id are
// sourced from the reservation row inside the primitive (the hold wrote them), so only the served model,
// known here and nowhere in the row, is passed through.
func (p *Proxy) settleReservation(ctx context.Context, deliveredUSD float64, servedModel string) float64 {
	return p.settleReservationBasis(ctx, deliveredUSD, servedModel, "")
}

// settleReservationBasis is settleReservation plus the PRICE BASIS of deliveredUSD. priceBasis is
// "fallback" when the served model was absent from the catalog and the charge is therefore a derived
// guess; empty when the price was exact. It rides onto the spend row so the guess is auditable later.
func (p *Proxy) settleReservationBasis(ctx context.Context, deliveredUSD float64, servedModel, priceBasis string) float64 {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return 0 // no reservation on this request ⇒ the consumer was charged nothing (plain key / path off)
	}
	finalLXC := int64(0)
	if deliveredUSD > 0 {
		finalLXC = int64(math.Ceil(deliveredUSD / economy.LXCUSDValue * 1e6))
	}
	settledLXC, _, err := p.agentSpender.SettleLXCReservation(ctx, h.reservationID, finalLXC,
		economy.AgentDebitMeta{ServedModel: servedModel, PriceBasis: priceBasis})
	if err != nil {
		// Logged-and-swallowed — the response is already served. A failed settle leaves the hold, which the
		// stranded sweeper later REFUNDS (never over-charges): the customer is protected on the error path.
		// Return 0 charge ⇒ the royalty seam treats it as unfunded (deflationary; never mint on a failed bill).
		slog.Warn("economy: reservation settle failed (hold will be swept/refunded; royalty treated as unfunded)",
			slog.String("reservation", h.reservationID), slog.String("err", err.Error()))
		return 0
	}
	return float64(settledLXC) * economy.LXCUSDValue / 1e6 // the USD the consumer ACTUALLY paid
}

// settleReservationPooled is settleReservationBasis for a CROSS-TENANT POOLED hit: it settles the
// charge and puts the pooled-discount disclosure on the money row, so the customer's evidence lives
// in the ledger and not only in a response header they may never have looked at.
//
// It passes the LIST price and the RATE, and deliberately NOT the saving: the settle clamps the
// charge to the hold, so the saving is derived inside the insert from what was actually debited
// (economy.AgentDebitMeta.toSpendMap). Passing it here would create a second source of truth that
// disagrees with the amount on the very same row.
//
// The rate is stamped per-row because it is tunable at boot: reading it back from config to explain
// an old charge would silently re-price history.
func (p *Proxy) settleReservationPooled(ctx context.Context, chargedUSD float64, price pooledPrice) float64 {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return 0 // no reservation ⇒ the consumer was charged nothing (plain key / path off)
	}
	settledLXC, cashBackedLXC, err := p.agentSpender.SettleLXCReservation(ctx, h.reservationID, price.ChargedULXC,
		economy.AgentDebitMeta{ServedModel: price.modelForRow, PriceBasis: price.PriceBasis,
			PoolListULXC: price.ListULXC, PoolDiscountRate: price.Rate})
	if err != nil {
		slog.Warn("economy: pooled reservation settle failed (hold will be swept/refunded; royalty treated as unfunded)",
			slog.String("reservation", h.reservationID), slog.String("err", err.Error()))
		return 0
	}
	// ⚠ THIS RETURNS THE CASH-BACKED PORTION, NOT THE CHARGE. The customer is still billed the full
	// settledLXC — that is untouched and lives on the ledger row. What changes is the ROYALTY BASIS:
	// only the part of this spend that real money paid for may mint LENS. Credit that arrived by
	// admin grant or by converting earnings funds no royalty, because no cash entered the system.
	//
	// The two figures are returned separately rather than collapsed because they answer different
	// questions, and settleReservationBasis's charge return is read by recordDistillServes —
	// repurposing it there would have silently corrupted distill attribution.
	if settledLXC > 0 && cashBackedLXC < settledLXC {
		slog.Info("poolroyalty: royalty basis reduced to the cash-backed portion of this spend",
			slog.Int64("settled_ulxc", settledLXC), slog.Int64("cash_backed_ulxc", cashBackedLXC),
			slog.String("reservation", h.reservationID))
	}
	return float64(cashBackedLXC) * economy.LXCUSDValue / 1e6
}

// releaseReservation REFUNDS the held reservation in full (if any) — an own-cache hit (no upstream call, no
// contributor ⇒ free) or a serve that delivered nothing. No-op without a reservation on the context.
func (p *Proxy) releaseReservation(ctx context.Context, reason string) {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return
	}
	if err := p.agentSpender.ReleaseLXCReservation(ctx, h.reservationID, reason); err != nil {
		slog.Warn("economy: reservation release failed (hold will be swept/refunded)",
			slog.String("reservation", h.reservationID), slog.String("err", err.Error()))
	}
}
