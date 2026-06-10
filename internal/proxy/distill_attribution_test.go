package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// fakeAttribSink records RecordDistillServe calls (owner, requester, hash).
type fakeAttribSink struct {
	calls [][3]string
	err   error
}

func (f *fakeAttribSink) RecordDistillServe(_ context.Context, owner, requester, hash string) error {
	f.calls = append(f.calls, [3]string{owner, requester, hash})
	return f.err
}

// ── tryConvertBlock fact emission (the surfacing + the upstream gates) ──

// A consented cross-tenant serve surfaces an attribution fact; the producing
// (private) serve does not.
func TestDistillAttribution_EmittedOnConsentedCrossTenantServe(t *testing.T) {
	d := newScopedDistiller(t, &countingConv{}, true, map[string]bool{"wsA": true, "wsB": true})
	ctx := context.Background()
	doc := docBlockBytes("shared")

	_, _, factA, ok := d.tryConvertBlock(ctx, doc, nil, "wsA") // produce + pool (private)
	if !ok || factA != nil {
		t.Fatalf("producer: ok=%v fact=%v, want a private produce (no fact)", ok, factA)
	}
	_, _, factB, ok := d.tryConvertBlock(ctx, doc, nil, "wsB") // served wsA's pooled artifact
	if !ok || factB == nil {
		t.Fatalf("consumer: ok=%v fact=%v, want a consented-serve fact", ok, factB)
	}
	if factB.owner != "wsA" || factB.hash == "" {
		t.Fatalf("fact = %+v, want owner=wsA + a content hash", *factB)
	}
}

// No fact on a DENIED pooled hit (requester not opted in / global flag off) or a
// PRIVATE within-workspace serve.
func TestDistillAttribution_NotEmitted_DeniedOrPrivate(t *testing.T) {
	ctx := context.Background()
	doc := docBlockBytes("doc")

	t.Run("requester not opted in", func(t *testing.T) {
		d := newScopedDistiller(t, &countingConv{}, true, map[string]bool{"wsA": true}) // wsB absent
		_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA")
		_, _, fact, _ := d.tryConvertBlock(ctx, doc, nil, "wsB")
		if fact != nil {
			t.Fatalf("denied (requester not opted in) emitted a fact: %+v", *fact)
		}
	})
	t.Run("global flag off", func(t *testing.T) {
		d := newScopedDistiller(t, &countingConv{}, false, map[string]bool{"wsA": true, "wsB": true})
		_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA")
		_, _, fact, _ := d.tryConvertBlock(ctx, doc, nil, "wsB")
		if fact != nil {
			t.Fatalf("global-flag-off emitted a fact: %+v", *fact)
		}
	})
	t.Run("private within-workspace reuse", func(t *testing.T) {
		d := newScopedDistiller(t, &countingConv{}, false, nil)
		_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA")
		_, _, fact, _ := d.tryConvertBlock(ctx, doc, nil, "wsA") // private hit
		if fact != nil {
			t.Fatalf("private serve emitted a fact: %+v", *fact)
		}
	})
}

// TestDistillAttribution_SelfServeSkipped is the RED-first guard: a poolable
// producer re-hitting its OWN pooled artifact (owner == requester) must emit NO
// attribution fact. RED against a build without the `owner != wsID` skip (a
// self-serve writes a row); GREEN with it.
func TestDistillAttribution_SelfServeSkipped(t *testing.T) {
	d := newScopedDistiller(t, &countingConv{}, true, map[string]bool{"wsA": true})
	ctx := context.Background()
	doc := docBlockBytes("self")

	_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA") // wsA produces + pools (owner=wsA)
	// wsA re-requests the SAME doc → pooled-read serves wsA's own artifact
	// (MaybeAllowPooledHit(wsA,wsA) is true). Self-serve ⇒ NO fact.
	_, _, fact, ok := d.tryConvertBlock(ctx, doc, nil, "wsA")
	if !ok {
		t.Fatal("wsA self-serve: not ok")
	}
	if fact != nil {
		t.Fatalf("SELF-SERVE emitted an attribution fact: %+v — owner==requester must be skipped", *fact)
	}
}

// ── recordDistillServes (the post-flush write gating) ──

func TestRecordDistillServes_Gating(t *testing.T) {
	facts := []distillServeFact{{owner: "wsA", hash: "h1"}}

	t.Run("normal → written once", func(t *testing.T) {
		sink := &fakeAttribSink{}
		p := &Proxy{distillAttribSink: sink}
		p.recordDistillServes(context.Background(), "wsB", workspace.LoggingMetadata, facts)
		if len(sink.calls) != 1 || sink.calls[0] != [3]string{"wsA", "wsB", "h1"} {
			t.Fatalf("calls=%v, want one (wsA, wsB, h1)", sink.calls)
		}
	})
	t.Run("LoggingNone → suppressed (the row records a content hash)", func(t *testing.T) {
		sink := &fakeAttribSink{}
		p := &Proxy{distillAttribSink: sink}
		p.recordDistillServes(context.Background(), "wsB", workspace.LoggingNone, facts)
		if len(sink.calls) != 0 {
			t.Fatalf("LoggingNone wrote %v, want none", sink.calls)
		}
	})
	t.Run("empty owner → skipped (no owner stamp)", func(t *testing.T) {
		sink := &fakeAttribSink{}
		p := &Proxy{distillAttribSink: sink}
		p.recordDistillServes(context.Background(), "wsB", workspace.LoggingMetadata, []distillServeFact{{owner: "", hash: "h"}})
		if len(sink.calls) != 0 {
			t.Fatalf("empty-owner wrote %v, want none", sink.calls)
		}
	})
	t.Run("nil sink → no-op (inert)", func(t *testing.T) {
		(&Proxy{}).recordDistillServes(context.Background(), "wsB", workspace.LoggingMetadata, facts) // must not panic
	})
	t.Run("sink error → swallowed (void, serve unaffected)", func(t *testing.T) {
		sink := &fakeAttribSink{err: errors.New("db down")}
		p := &Proxy{distillAttribSink: sink}
		p.recordDistillServes(context.Background(), "wsB", workspace.LoggingMetadata, facts) // must not panic/propagate
	})
}
