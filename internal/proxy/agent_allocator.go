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
	// Reservation lifecycle (billing redesign) — satisfied by *economy.DualTokenStore.
	ReserveLXCForAgent(ctx context.Context, scopedKeyID, workspaceID, reservationID string, heldLXC int64, meta economy.AgentDebitMeta) error
	SettleLXCReservation(ctx context.Context, reservationID string, finalLXC int64, meta economy.AgentDebitMeta) error
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
		return false // nothing to charge against (unknown model / empty) → allow, like the LXC gate
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
// seam so the hold is released PROMPTLY, not on a timer (the sweeper is for crashes only). No-op if there is
// no reservation on the context (non-agent request, or reservation path off). deliveredUSD is converted to
// µLXC (ceil); the primitive clamps to [0, held] so a mis-estimate cannot over-bill.
func (p *Proxy) settleReservation(ctx context.Context, deliveredUSD float64) {
	h, ok := reservationFrom(ctx)
	if !ok || p.agentSpender == nil {
		return
	}
	finalLXC := int64(0)
	if deliveredUSD > 0 {
		finalLXC = int64(math.Ceil(deliveredUSD / economy.LXCUSDValue * 1e6))
	}
	if err := p.agentSpender.SettleLXCReservation(ctx, h.reservationID, finalLXC, economy.AgentDebitMeta{RequestID: h.requestID}); err != nil {
		// Logged-and-swallowed — the response is already served. A failed settle leaves the hold, which the
		// stranded sweeper later REFUNDS (never over-charges): the customer is protected on the error path.
		slog.Warn("economy: reservation settle failed (hold will be swept/refunded)",
			slog.String("reservation", h.reservationID), slog.String("err", err.Error()))
	}
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
