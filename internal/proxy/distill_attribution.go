package proxy

import (
	"context"
	"log/slog"

	"github.com/talyvor/lens/internal/workspace"
)

// distill_attribution.go — the S1 distill attribution WRITE: a durable,
// MINT-FREE record of a CONSENTED cross-tenant pooled-distill serve (who
// contributed the artifact, who was served, the content hash).
//
// THE SAFETY IS STRUCTURAL (same shape as capturePattern / the pooled mint):
// recordDistillServes returns NOTHING — a void, post-serve, error-swallowed,
// detached call cannot block, delay, fail, or alter any request. The sink
// interface exposes ONLY RecordDistillServe — there is no credit/earn method, so
// this path cannot mint by construction.
//
// GATED on ALL of: (1) the three-switch distill-pooling consent already
// authorized the cross-tenant serve (the fact is emitted ONLY on the consented
// branch of tryConvertBlock), (2) owner != requester (self-serve skipped
// upstream), and (3) the requester's logging policy is not None (the row records
// a content hash). Default-off: a nil sink ⇒ no call at all.

// distillServeFact is a consented cross-tenant distill serve surfaced up from
// MaybeDistill so serve() can record it post-flush. owner is the artifact's
// contributing workspace; the requester is the serving request's workspace.
type distillServeFact struct {
	owner string
	hash  string
}

// distillAttributionSink is the minimal write surface the proxy depends on — one
// method, persist-only. *distillattrib.Store satisfies it. Deliberately exposes
// NO credit/earn method (cannot mint by construction).
type distillAttributionSink interface {
	RecordDistillServe(ctx context.Context, owner, requester, contentHash string) error
}

// recordDistillServes writes S1 attribution rows for the consented cross-tenant
// serves in `facts`. VOID by design — it cannot affect the response. Called
// POST-SERVE on a detached context (so client cancellation can't drop the write,
// and the write can't touch the serve). Errors are logged-and-swallowed.
//
// SUPPRESSED under LoggingNone: the row records a content hash, so it honors the
// same no-logging posture as the spend/capture writes. Empty-owner facts are
// skipped defensively (a pre-feature pooled entry with no owner stamp — though
// the consent gate already refuses to serve those cross-tenant).
func (p *Proxy) recordDistillServes(ctx context.Context, requester string, loggingPolicy workspace.LoggingPolicy, facts []distillServeFact) {
	if p == nil || p.distillAttribSink == nil || len(facts) == 0 {
		return
	}
	if loggingPolicy == workspace.LoggingNone {
		return // the row records a content hash; honor the no-logging posture
	}
	for _, f := range facts {
		if f.owner == "" {
			continue // no owner stamp ⇒ nothing to attribute (defensive)
		}
		if err := p.distillAttribSink.RecordDistillServe(context.WithoutCancel(ctx), f.owner, requester, f.hash); err != nil {
			slog.Warn("distill attribution: write failed (observational; serve unaffected)",
				slog.String("owner", f.owner),
				slog.String("requester", requester),
				slog.String("err", err.Error()),
			)
		}
	}
}
