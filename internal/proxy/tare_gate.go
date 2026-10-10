package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/tare"
	"github.com/talyvor/lens/internal/workspace"
)

// shouldTare applies the workspace policy + per-request header to Tare (internal/tare), the
// context-reduction layer. B6.5. Same shape as shouldCompress: the workspace must allow it AND
// (the policy is always-on and the request does not carry X-Talyvor-Tare: false, OR the policy is
// opt-in and the request carries X-Talyvor-Tare: true). Every failure direction is OFF — no
// workspace manager, an unregistered workspace, a stale cache — and a header alone can never turn
// it on.
func (p *Proxy) shouldTare(r *http.Request, wsID string) bool {
	if p.workspaceManager == nil {
		return false
	}
	switch p.workspaceManager.GetTarePolicy(wsID) {
	case workspace.TareAlways:
		return r == nil || !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Talyvor-Tare")), "false")
	case workspace.TareOptIn:
		return r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Talyvor-Tare")), "true")
	default: // TareDisabled
		return false
	}
}

// SetTareModel installs Tare phase 2a, the compression model (B27.35). It runs only for a workspace
// whose tare_model opt-in is on, and only where every phase-1 reducer refused.
func (p *Proxy) SetTareModel(r tare.Reduction) { p.tareModel = r }

// tareModelFor is phase 2a's reducer for this workspace, or nil: no model installed, no workspace
// manager, or the workspace has not opted in. Every failure direction is OFF.
func (p *Proxy) tareModelFor(wsID string) tare.Reduction {
	if p.tareModel == nil || p.workspaceManager == nil || !p.workspaceManager.GetTareModel(wsID) {
		return nil
	}
	return p.tareModel
}

// tareReduce runs Tare on a chat request body. Only the NEWEST message's content can change —
// tare.PrefixStable splices it and re-checks that every byte before it is untouched, so the
// provider's prompt cache over the system prompt and history still hits. phase2, when not nil, is
// tried only after every phase-1 reducer refused, and is metered as kind prose. ok=false means the
// body is returned exactly as given.
func tareReduce(ctx context.Context, body []byte, phase2 tare.Reduction) (out []byte, kind tare.Kind, tokensIn, tokensOut int, ok bool) {
	for _, r := range tare.Phase1 {
		reduced, tin, tout, err := tare.NewPrefixStable(r.New(nil), r.Kind).Reduce(ctx, body, r.Kind)
		if err == nil && len(reduced) < len(body) {
			return reduced, r.Kind, tin, tout, true
		}
	}
	if phase2 != nil {
		reduced, tin, tout, err := tare.NewPrefixStable(phase2, tare.KindProse).Reduce(ctx, body, tare.KindProse)
		if err == nil && len(reduced) < len(body) {
			return reduced, tare.KindProse, tin, tout, true
		}
	}
	return body, "", 0, 0, false
}

// tareTraceSink keeps a workspace's prose for Tare phase 2b's training set (workspace.Manager.RecordTareTrace).
type tareTraceSink interface{ RecordTareTrace(wsID, text string) }

// SetTareTraining installs where Tare phase 2b's training traces go (B27.36).
func (p *Proxy) SetTareTraining(s tareTraceSink) { p.tareTraces = s }

// maxTareTraceBytes: past phase 2a's token budget the model never reads the text, so it teaches nothing.
const maxTareTraceBytes = 32 << 10

// keepTareTrace keeps the newest message's text for training when every phase-1 reducer refuses it — the text a
// compressor works on — for a workspace that opted in or is synthetic test traffic. Never a temporary chat's
// (X-Talyvor-Cache-Store: off) and never one kept out of the shared pool (X-Talyvor-Pool: off).
func (p *Proxy) keepTareTrace(ctx context.Context, r *http.Request, wsID string, body []byte) {
	if p.tareTraces == nil || p.workspaceManager == nil || r == nil || !p.workspaceManager.CollectsTareTraining(wsID) ||
		strings.EqualFold(r.Header.Get("X-Talyvor-Cache-Store"), "off") || strings.EqualFold(r.Header.Get("X-Talyvor-Pool"), "off") {
		return
	}
	// shortcut: runs phase 1 a second time when Tare is on; fold into tareReduce if it shows in a profile.
	if _, _, _, _, reduced := tareReduce(ctx, body, nil); reduced {
		return
	}
	var c captureText
	_, _, _, _ = tare.NewPrefixStable(&c, tare.KindProse).Reduce(ctx, body, tare.KindProse)
	if len(c) > 0 && len(c) <= maxTareTraceBytes {
		p.tareTraces.RecordTareTrace(wsID, string(c))
	}
}

// captureText is a Reduction that changes nothing and remembers what it was given.
type captureText []byte

func (c *captureText) Reduce(_ context.Context, content []byte, _ tare.Kind) ([]byte, int, int, error) {
	*c = append((*c)[:0], content...)
	return content, 0, 0, nil
}
