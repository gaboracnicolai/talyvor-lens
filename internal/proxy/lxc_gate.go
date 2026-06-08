package proxy

import (
	"context"
	"log/slog"
	"math"

	"github.com/talyvor/lens/internal/alerts"
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
	GetLXCBalance(ctx context.Context, workspaceID string) (float64, error)
}

// SetLXCGate wires the LXC gating reader + its enable flag (read per-call). The
// proxy holds both as optional, nil-safe fields. The coherence rule also reads
// the existing lxcShadowEnabled (set by SetLXCSpendSink).
func (p *Proxy) SetLXCGate(reader lxcBalanceReader, enabled func() bool) {
	p.lxcGate = reader
	p.lxcGatingEnabled = enabled
}

// lxcEstimate is the input-only pre-serve LXC cost estimate (output=0),
// converted at the fixed peg, 6-dp — mirrors the budget gate's estCost.
func lxcEstimate(model, prompt string) float64 {
	estUSD := alerts.CostUSD(model, len(prompt)/4, 0)
	return math.Round(estUSD/economy.LXCUSDValue*1e6) / 1e6
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
