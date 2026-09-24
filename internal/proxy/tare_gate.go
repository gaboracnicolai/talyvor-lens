package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/tare"
	"github.com/talyvor/lens/internal/workspace"
)

// shouldTare applies the workspace policy + per-request opt-in to Tare (internal/tare), the
// context-reduction layer. B6.5. Same shape as shouldCompress: the workspace must allow it AND
// (the policy is always-on OR the request carries X-Talyvor-Tare: true). Every failure direction is
// OFF — no workspace manager, an unregistered workspace, a stale cache — and a header alone can
// never turn it on.
func (p *Proxy) shouldTare(r *http.Request, wsID string) bool {
	if p.workspaceManager == nil {
		return false
	}
	switch p.workspaceManager.GetTarePolicy(wsID) {
	case workspace.TareAlways:
		return true
	case workspace.TareOptIn:
		return r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Talyvor-Tare")), "true")
	default: // TareDisabled
		return false
	}
}

// tareReducers are Tare's phase-1 reducers in the order they are tried on the newest message. Each
// one REFUSES content that is not its kind, so the first that shrinks the message wins; prose, and
// anything else none of them recognises, goes upstream unchanged.
var tareReducers = []struct {
	kind tare.Kind
	make func() tare.Reduction
}{
	{tare.KindJSON, func() tare.Reduction { return tare.NewJSONReducer() }},
	{tare.KindCode, func() tare.Reduction { return tare.NewGoBodyTrimmer() }},
	{tare.KindCode, func() tare.Reduction { return tare.NewTSBodyTrimmer() }},
	{tare.KindLog, func() tare.Reduction { return tare.NewLogCollapse() }},
}

// tareReduce runs Tare on a chat request body. Only the NEWEST message's content can change —
// tare.PrefixStable splices it and re-checks that every byte before it is untouched, so the
// provider's prompt cache over the system prompt and history still hits. ok=false means the body
// is returned exactly as given.
func tareReduce(ctx context.Context, body []byte) (out []byte, kind tare.Kind, tokensIn, tokensOut int, ok bool) {
	for _, r := range tareReducers {
		reduced, tin, tout, err := tare.NewPrefixStable(r.make(), r.kind).Reduce(ctx, body, r.kind)
		if err == nil && len(reduced) < len(body) {
			return reduced, r.kind, tin, tout, true
		}
	}
	return body, "", 0, 0, false
}
