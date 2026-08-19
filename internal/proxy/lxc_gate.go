package proxy

import (
	"context"
	"log/slog"
	"math"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// lxc_gate.go — LXC GATING (Phase-2 Stage 2.4/2.5): the pre-serve check that
// BLOCKS a request (402) when the workspace's LXC balance can't cover the
// estimated cost. This is the first gate that can alter whether a request
// SUCCEEDS — it ships behind its own default-off flag (LXCGatingEnabled) and is
// inert until deliberately enabled.
//
// CHECK-before-serve (not debit-before-serve): a no-lock read of the LXC
// balance (the existing GetLXCBalance), decide block/allow, serve, and let the
// EXISTING post-serve shadow debit (shadowSpendLXC) book the real cost. No
// reservation, no refund primitive (refund is structurally forbidden by
// TestNoReverseConversionPath — untouched).
//
// COHERENCE — gating is INERT unless shadow is ALSO on. Gating means "block
// when unaffordable, then debit," but the only serving-path debit is
// shadowSpendLXC (gated on lxcShadowEnabled). Gating without shadow would block
// requests yet never move LXC — blocking with no accounting on a frozen
// balance. So the gate requires BOTH lxcGatingEnabled() AND lxcShadowEnabled():
// a two-flag staging (shadow=observe, shadow+gating=enforce) where a half-config
// fails safe toward serving.
//
// PRE-SERVE ESTIMATE is input-only (output unknown pre-call), so the gate
// UNDER-blocks by design — exactly like the budget gate's
// estCost = alerts.CostUSD(model, len(prompt)/4, 0). The true output-inclusive
// cost books post-serve via the shadow debit.
//
// FAIL-OPEN on a balance-read error: log and ALLOW (mirrors the workspace
// spend cap — "rather under-enforce than fail-closed on a transient DB error").
// A fail-open admit is still booked post-serve by the shadow debit — bounded
// slack, not a free call.

// lxcBalanceReader is the minimal read surface the gate needs — one method,
// a no-lock balance read. *economy.DualTokenStore.GetLXCBalance satisfies it.
// Deliberately separate from lxcSpendSink so the shadow path stays untouched.
type lxcBalanceReader interface {
	GetLXCBalance(ctx context.Context, workspaceID string) (int64, error)
}

// SetLXCGate wires the LXC gating reader + its enable flag (read per-call). The
// proxy holds both as optional, nil-safe fields. The coherence rule also reads
// the existing lxcShadowEnabled (set by SetLXCSpendSink).
func (p *Proxy) SetLXCGate(reader lxcBalanceReader, enabled func() bool) {
	p.lxcGate = reader
	p.lxcGatingEnabled = enabled
}

// lxcEstimate is the input-only pre-serve LXC cost estimate (output=0),
// converted at the fixed peg — in µLXC (SEC-2). It gates a CHARGE, so it rounds
// UP (ceil): a conservative estimate never under-reserves a sub-µLXC.
//
// ⚠ IT PRICES VIA CostUSDResolved, NOT CostUSD, for the same reason reserveEstimateLXC does — and this
// one is easier to miss, because both of its callers treat a zero as PERMISSION:
//
//	agentAllocationBlocks — the immediate-DEBIT path taken whenever p.reservationActive() is false
//	                        (proxy.go:799). `estLXC <= 0` returned false = serve, WITHOUT DEBITING.
//	                        LENS_LXC_AGENT_ALLOCATION_ENABLED defaults TRUE, so this was live.
//	lxcGateBlocks         — the balance ADMISSION gate. A zero can never exceed a balance, so an
//	                        unknown model was never gated, even on a workspace with no credit.
//
// PurposeCharge (the FLOOR) is right for both. This debit is immediate and never refunded, so
// over-charging on a rate we admit is a guess is the indefensible direction; and for the gate a floor
// starts blocking a zero-balance workspace without over-blocking a paying one.
//
// An EMPTY prompt still yields 0, correctly — zero tokens really is zero cost.
//
// ⚠ BUT IT IS NOT THE ONLY CASE, WHICH IS WHAT THIS SAID ("for that case and only that case"). The
// ceil above is applied to the µLXC conversion; the TOKEN conversion one line below is len(prompt)/4,
// which FLOORS. Every prompt of 1, 2 or 3 bytes is therefore zero tokens and zero µLXC on ANY model,
// known or unknown, and takes both callers' `<= 0` branch: agentAllocationBlocks serves it without
// debiting (measured against an exhausted sub-budget on real Postgres — see
// TestRealPG_AnExhaustedAgentCeilingStillServesASubFourBytePrompt) and lxcGateBlocks admits it on a
// workspace with no credit, since a zero can never exceed a balance. The boundary is pinned from both
// sides in lxc_estimate_short_prompt_test.go; closing it re-prices or refuses live traffic and is a
// decision rather than a repair.
func lxcEstimate(model, prompt string) int64 {
	estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)
	if prov == catalog.ProvenanceFallback && len(prompt) > 0 {
		alerts.WarnUnpricedModel(model, catalog.PurposeCharge, estUSD)
	}
	return int64(math.Ceil(estUSD / economy.LXCUSDValue * 1e6)) // µLXC
}

// reserveEstimateLXC is the CONSERVATIVE (output-aware) pre-serve HOLD, in µLXC: input tokens from the
// prompt PLUS a BOUNDED output allowance (maxOutTokens), priced on the requested model, ceil. Unlike the
// input-only lxcEstimate (which under-holds against an output-inclusive charge and would leak the ceiling
// by exactly the output cost), this is a true upper bound on the delivered cost, so the settle only ever
// refunds. maxOutTokens is BOUNDED by the caller (explicit max_tokens, else a sane cap — never catalog max).
//
// ⚠ IT PRICES VIA CostUSDResolved, NOT CostUSD. An unknown model used to price at 0 here, which made
// heldLXC 0, which made agentReserveBlocks return "no hold" — so the request carried no reservation, the
// settle had nothing to charge against, and the sub-budget ceiling was never consulted. A hold falls back
// to the provider's most expensive known model: over-holding is refunded by the settle, under-holding
// leaks the ceiling, so HIGH is the conservative direction here.
func reserveEstimateLXC(model, prompt string, maxOutTokens int) int64 {
	if maxOutTokens < 0 {
		maxOutTokens = 0
	}
	estUSD, _ := alerts.CostUSDResolved(model, catalog.PurposeHold, len(prompt)/4, 0, 0, maxOutTokens)
	return int64(math.Ceil(estUSD / economy.LXCUSDValue * 1e6)) // µLXC
}

// lxcGateBlocks reports whether the request should be BLOCKED (true) for
// insufficient LXC. The caller (after the budget gate, before the upstream
// call) does writeError(402)+return on true, so "upstream never called" is
// structural by placement. Returns false (allow) whenever the gate is inert,
// the estimate is zero, or the balance read errors (fail-open).
func (p *Proxy) lxcGateBlocks(ctx context.Context, workspaceID, model, prompt string, loggingPolicy workspace.LoggingPolicy) bool {
	if p == nil || p.lxcGate == nil || p.lxcGatingEnabled == nil || !p.lxcGatingEnabled() {
		return false
	}
	// COHERENCE: inert unless shadow is also on (no block without accounting)...
	if p.lxcShadowEnabled == nil || !p.lxcShadowEnabled() {
		return false
	}
	// ...AND inert for LoggingNone, where the shadow debit never fires — so the
	// gate's live-condition exactly matches the debit's fire-condition. Blocking
	// a LoggingNone workspace would freeze it on a balance that never moves.
	if loggingPolicy == workspace.LoggingNone {
		return false
	}
	estLXC := lxcEstimate(model, prompt)
	if estLXC <= 0 {
		return false // nothing to charge against (unknown model / empty) → allow
	}
	balance, err := p.lxcGate.GetLXCBalance(ctx, workspaceID)
	if err != nil {
		// FAIL-OPEN — allow and log, mirroring the spend cap. The post-serve
		// shadow debit still books the real cost (bounded slack, not free).
		slog.Warn("economy: LXC gate balance read failed (failing open; request allowed)",
			slog.String("workspace", workspaceID),
			slog.String("err", err.Error()),
		)
		return false
	}
	return balance < estLXC
}
