package workspace

import (
	"context"
	"fmt"
)

// CompressionPolicy controls whether the request-path PROMPT REWRITER
// (internal/compressor) rewrites this workspace's prompts before they are sent
// upstream. It mirrors DistillPolicy — a stored policy plus a per-request header
// — because that pattern already exists here and already carries the same shape
// of decision.
//
// ⚠ IT IS OFF BY DEFAULT, AND THE DEFAULT IS THE WHOLE POINT OF THE TYPE. The
// rewriter shipped UNCONDITIONALLY on every non-streaming request from the day it
// was wired (cmd/lens/main.go had no flag and no condition; proxy.serve called
// Compress and forwarded the result). Measured against this repo's own committed
// corpora, that was a trade with nothing on the benefit side:
//
//   - 0 of 308 prompts modified across the seven poolsafety rephrase/danger
//     corpora — aggregate 2717 → 2717 tokens, 0.000%. The technique list is a
//     fixed regex set for conversational filler ("please", "in order to"), which
//     API traffic does not carry.
//   - 8 of 8 prompts modified in poolsafety.Corpus(), the corpus that stands in
//     for real coding-agent traffic (a system preamble plus an unfenced code
//     diff). The space-run collapse rewrites LEADING INDENTATION when the code is
//     not inside a ``` fence, so two Python nesting levels arrive as one.
//   - four content-corruption cases where the answer changes, not the cost
//     (see internal/compressor/reach_test.go, which pins each by name).
//
// A workspace that wants it turns it on explicitly. The SEAM is kept because a
// real reduction layer will live here; the current TECHNIQUE is what measured
// worthless.
//
// ⚠ WHAT THIS DOES NOT TOUCH — MEASURED, NOT ASSUMED: no money seam on the serving
// path reads the rewriter's output. All four read `prompt`, the ORIGINAL, and the
// three pre-serve ones run before the rewrite exists at all:
//
//	proxy.go:849   budget gate         alerts.CostUSD(model, len(prompt)/4, 0)
//	proxy.go:861   LXC gate            p.lxcGateBlocks(..., prompt, ...)
//	proxy.go:879   reservation hold    p.agentReserveBlocks(..., prompt, ...)   (:892 the non-reservation form)
//	proxy.go:1239  ── the rewrite is created here ──
//	proxy.go:1620  post-serve estimate inT := len(prompt)/4
//
// Turning the rewriter off therefore moves no hold and no charge. Asserted end to
// end through the wire by proxy.TestBilling_TheEstimateIsTheSameWithTheGateOpenOr
// Closed, which also pins the other half of that fact: an opted-IN workspace is
// billed for the bytes it did NOT send. Distill is the layer that does shrink a
// prompt before those seams (see DistillPolicy); the two are not interchangeable.
type CompressionPolicy string

const (
	// CompressionDisabled never rewrites — fully inert. The default.
	CompressionDisabled CompressionPolicy = "disabled"
	// CompressionOptIn rewrites ONLY when the request also carries
	// X-Talyvor-Compress: true (per-request opt-in).
	CompressionOptIn CompressionPolicy = "opt_in"
	// CompressionAlways rewrites every non-streaming request (no header needed).
	CompressionAlways CompressionPolicy = "always"
)

// DefaultCompressionPolicy is what a workspace gets when it sets NO policy.
// DISABLED, unlike DefaultDistillPolicy: distilling a document lowers the
// customer's bill and degrades harmlessly, while this rewriter's measured saving
// is 0.000% and its measured failure mode is a silently altered prompt.
const DefaultCompressionPolicy = CompressionDisabled

// normalizeCompressionPolicy resolves a stored/supplied policy:
//   - an explicit valid value is honoured, so a workspace that opted in stays on;
//   - EMPTY (unset) resolves to DefaultCompressionPolicy;
//   - anything else (a typo, a truncated write) fails SAFE to CompressionDisabled,
//     so a misconfiguration can never start rewriting prompts.
//
// ⚠ THE LAST TWO BRANCHES ARE INDISTINGUISHABLE TODAY and are kept apart on
// purpose: DefaultCompressionPolicy IS CompressionDisabled, so no test can score
// one against the other while that holds. They encode different intents and
// diverge the moment the default moves off disabled — which is exactly when the
// fail-safe would start to matter. compression_policy_test.go pins the default
// against the literal "disabled" so that move cannot happen silently.
func normalizeCompressionPolicy(p CompressionPolicy) CompressionPolicy {
	switch p {
	case CompressionDisabled, CompressionOptIn, CompressionAlways:
		return p
	case "":
		return DefaultCompressionPolicy
	default:
		return CompressionDisabled
	}
}

const updateCompressionPolicySQL = `UPDATE workspaces
SET compression_policy = $2, updated_at = NOW()
WHERE id = $1`

// GetCompressionPolicy returns the workspace's compression policy, or the safe
// CompressionDisabled default when the workspace isn't registered. Hot-path code
// (proxy.serve) calls this on every non-streaming request — lock-light, never
// reaches the DB.
func (m *Manager) GetCompressionPolicy(wsID string) CompressionPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Fail closed on prolonged staleness: an unconfirmed opt-in could be stale, and
	// the cost of being wrong here is a rewritten prompt nobody asked for.
	if m.staleBeyondBoundLocked() {
		return CompressionDisabled
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return normalizeCompressionPolicy(ws.CompressionPolicy)
	}
	return CompressionDisabled
}

// SetCompressionPolicy updates the in-memory cache and the DB row. Policy changes
// take effect on the very next request.
func (m *Manager) SetCompressionPolicy(ctx context.Context, wsID string, policy CompressionPolicy) error {
	policy = normalizeCompressionPolicy(policy)
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.CompressionPolicy = policy
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateCompressionPolicySQL, wsID, string(policy)); err != nil {
			return fmt.Errorf("workspace: update compression_policy: %w", err)
		}
	}
	return nil
}
