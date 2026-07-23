package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"

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
