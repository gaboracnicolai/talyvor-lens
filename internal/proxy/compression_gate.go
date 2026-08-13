package proxy

import (
	"net/http"
	"strings"

	"github.com/talyvor/lens/internal/workspace"
)

// shouldCompress applies the workspace policy + per-request opt-in rules to the
// PROMPT REWRITER (internal/compressor): a prompt is rewritten only if the
// workspace allows it AND (the policy is always-on OR the request carries
// X-Talyvor-Compress: true). It mirrors distillIntegration.shouldDistill.
//
// ⚠ WHAT THIS GATE CHANGED, AND WHY IT IS NOT A REFACTOR: before it, proxy.serve
// called Compress unconditionally on every non-streaming request and rebuildBody
// forwarded the REWRITE upstream — with no env var, no policy and no header
// anywhere in the tree. Measured over this repo's own committed corpora, that
// bought 0.000% on 308 prompts and rewrote 8 of 8 prompts in poolsafety.Corpus(),
// the corpus that models real coding-agent traffic; the space-run collapse
// re-indents code that is not inside a ``` fence, so two Python nesting levels
// arrive at the provider as one. compression_gate_test.go pins the direction
// through the WIRE, not through Compress.
//
// The failure direction is OFF: no workspace manager, an unregistered workspace, a
// stale cache or a garbage stored value all answer "do not rewrite". The one thing
// a client cannot do is turn it on by header alone.
//
// ⚠⚠ BUT THIS GATE DECIDES WHETHER THE REWRITER RUNS, NOT WHAT THE CALLER
// RECEIVES, AND THE TWO COME APART AT THE CACHE. proxy.go#serve builds cachePrompt
// from the prompt BEFORE the rewrite, so a response produced from the REWRITTEN
// prompt is stored under the UNREWRITTEN one's key with nothing recording that
// the rewriter ran. One request carrying X-Talyvor-Compress: true therefore
// answers every later identical request in that workspace, header or no header —
// so the per-request opt-in is per-request in its CONSENT and workspace-wide in
// its EFFECT, and flipping a policy back to `disabled` does not restore the
// caller's own bytes for anything already cached. Measured through the wire, not
// inferred: TestCompressionCache_AnOptedInRewriteAnswersARequestThatDidNotOptIn.
// Not fixed — the cache key is the hit rate W2.1 measured, so splitting it is a
// decision about the pooling economics rather than a repair.
//
// ⚠ X-Talyvor-Compress IS NOT BROWSER-REACHABLE, measured not assumed:
// api.CORSMiddleware emits Access-Control-Allow-Headers = "Authorization,
// Content-Type, X-Request-ID, X-Talyvor-Key" and nothing else, so a cross-origin
// preflight refuses this header — for a browser client `opt_in` is indistinguish-
// able from `disabled` and the WORKSPACE policy is the only lever. This is the
// same hole COORDINATION.md already records for X-Talyvor-Distill, on the same
// coordinated file, so it is documented here rather than patched unilaterally. It
// is inert today: LENS_CORS_ALLOWED_ORIGINS is unset by default, CORS is then a
// no-op, and every caller is server-to-server. Pinned by
// api.TestCORSMiddleware_PerRequestOptInHeadersAreNotAllowlisted, which FAILS the
// day the allowlist gains the header — that failure is the signal to delete this
// paragraph, not to edit the test.
func (p *Proxy) shouldCompress(r *http.Request, wsID string) bool {
	if p.workspaceManager == nil {
		return false
	}
	switch p.workspaceManager.GetCompressionPolicy(wsID) {
	case workspace.CompressionAlways:
		return true
	case workspace.CompressionOptIn:
		return r != nil && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Talyvor-Compress")), "true")
	default: // CompressionDisabled
		return false
	}
}
