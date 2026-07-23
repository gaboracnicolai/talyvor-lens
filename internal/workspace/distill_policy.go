package workspace

import (
	"context"
	"fmt"
)

// DistillPolicy controls whether the request-path DISTILL integration converts
// a document in this workspace's requests. It mirrors LoggingPolicy.
//
// Distill is ON BY DEFAULT (DefaultDistillPolicy = DistillAlways). It is the one
// savings feature that reaches the CUSTOMER's charge — it shrinks a document to
// compact Markdown BEFORE the pre-serve cost estimate (proxy.go), so the customer
// is billed on the smaller prompt — and it degrades safely to today's behaviour:
// with no worker binary configured a conversion simply fails and the ORIGINAL
// body is served unchanged (proxy MaybeDistill never fails a request on a
// conversion error). A workspace turns it off with an explicit DistillDisabled.
type DistillPolicy string

const (
	// DistillDisabled never distills — fully inert. An explicit opt-out.
	DistillDisabled DistillPolicy = "disabled"
	// DistillOptIn distills a document request ONLY when it also sends the
	// X-Talyvor-Distill header (per-request opt-in).
	DistillOptIn DistillPolicy = "opt_in"
	// DistillAlways distills whenever a document is present (no header needed).
	DistillAlways DistillPolicy = "always"
)

// DefaultDistillPolicy is what a workspace gets when it sets NO policy — the
// on-by-default state. Distilling documents is a saving that lowers the
// customer's bill and degrades harmlessly when unconfigured (see above), so the
// default is DistillAlways rather than the former inert DistillDisabled.
const DefaultDistillPolicy = DistillAlways

// normalizeDistillPolicy resolves a stored/supplied policy:
//   - an explicit valid value (disabled / opt_in / always) is honoured — so an
//     existing workspace's stored 'disabled' stays off, never flipped by the
//     default;
//   - an EMPTY value (unset — the workspace wants the default) resolves to
//     DefaultDistillPolicy, so a freshly-registered workspace is on by default;
//   - anything else (a typo / garbage) fails SAFE to DistillDisabled, so a
//     misconfiguration never silently distills.
func normalizeDistillPolicy(p DistillPolicy) DistillPolicy {
	switch p {
	case DistillDisabled, DistillOptIn, DistillAlways:
		return p
	case "":
		return DefaultDistillPolicy
	default:
		return DistillDisabled
	}
}

const updateDistillPolicySQL = `UPDATE workspaces
SET distill_policy = $2, updated_at = NOW()
WHERE id = $1`

// GetDistillPolicy returns the workspace's distill policy, or the safe
// DistillDisabled default when the workspace isn't registered. Hot-path code
// (proxy.serve) calls this on every request — lock-light, never reaches the DB.
func (m *Manager) GetDistillPolicy(wsID string) DistillPolicy {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Fail closed on prolonged staleness: stop distilling when the policy can't be
	// confirmed (an unconfirmed opt-in could be stale-permissive).
	if m.staleBeyondBoundLocked() {
		return DistillDisabled
	}
	if ws, ok := m.workspaces[wsID]; ok {
		return normalizeDistillPolicy(ws.DistillPolicy)
	}
	return DistillDisabled
}

// SetDistillPolicy updates the in-memory cache and the DB row. Policy changes
// take effect on the very next request.
func (m *Manager) SetDistillPolicy(ctx context.Context, wsID string, policy DistillPolicy) error {
	policy = normalizeDistillPolicy(policy)
	m.mu.Lock()
	ws, ok := m.workspaces[wsID]
	if ok {
		ws.DistillPolicy = policy
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("workspace: %q not registered", wsID)
	}
	if m.pool != nil {
		if _, err := m.pool.Exec(ctx, updateDistillPolicySQL, wsID, string(policy)); err != nil {
			return fmt.Errorf("workspace: update distill_policy: %w", err)
		}
	}
	return nil
}
