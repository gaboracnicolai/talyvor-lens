package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/cache_pooling"
	"github.com/talyvor/lens/internal/distill"
)

// countingConv returns DISTINCT markdown per call ("converted-N"), so a test can
// tell a fresh conversion from a cache serve: if workspace B receives workspace
// A's "converted-1", it was served A's artifact (the cross-tenant flow #129
// closes by default).
type countingConv struct{ calls int }

func (c *countingConv) Convert(_ context.Context, _ []byte, format distill.Format) (distill.Result, error) {
	c.calls++
	return distill.Result{Markdown: fmt.Sprintf("converted-%d", c.calls), Format: format}, nil
}

func docBlockBytes(content string) map[string]any {
	return map[string]any{"type": "document", "source": map[string]any{
		"type": "base64", "media_type": "application/pdf",
		"data": base64.StdEncoding.EncodeToString([]byte(content)),
	}}
}

// newScopedDistiller builds a distillIntegration over a REAL (miniredis) distill
// cache + a configurable distill-pooling gate. globalOn = LENS_DISTILL_POOLABLE_ENABLED;
// poolable = each workspace's distill_poolable opt-in.
func newScopedDistiller(t *testing.T, conv distill.IsolatedConverter, globalOn bool, poolable map[string]bool) *distillIntegration {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	gate := cache_pooling.New(
		func() bool { return globalOn },
		func(ws string) bool { return poolable[ws] },
	)
	return &distillIntegration{
		converter: conv,
		cache:     cache.NewDistillCache(rc, time.Hour),
		poolGate:  gate,
	}
}

// TestDistill_PrivateByDefault_NoCrossTenantServe is the headline S0 guard: with
// the gate off (the default), one workspace's distill artifact is NEVER served
// to another — the previously-ungated cross-tenant flow is closed — while
// within-workspace reuse is unchanged (serve-neutrality).
func TestDistill_PrivateByDefault_NoCrossTenantServe(t *testing.T) {
	conv := &countingConv{}
	d := newScopedDistiller(t, conv, false, nil) // gate fully off (default)
	ctx := context.Background()
	doc := docBlockBytes("the-same-document-bytes")

	mdA, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsA")
	if !ok || mdA != "converted-1" {
		t.Fatalf("wsA produce: ok=%v md=%q", ok, mdA)
	}

	// wsB sends the SAME bytes — must NOT receive wsA's artifact; it converts fresh.
	mdB, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsB")
	if !ok {
		t.Fatal("wsB: not ok")
	}
	if mdB == "converted-1" {
		t.Fatal("LEAK: wsB was served wsA's distill artifact (ungated cross-tenant flow)")
	}
	if mdB != "converted-2" {
		t.Fatalf("wsB must convert fresh, got %q", mdB)
	}
	if conv.calls != 2 {
		t.Fatalf("no cross-tenant reuse → 2 conversions, got %d", conv.calls)
	}

	// Serve-neutrality: wsA re-serving the same bytes hits its OWN private cache
	// (within-workspace reuse, unchanged from pre-S0) — no new conversion.
	mdA2, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsA")
	if !ok || mdA2 != "converted-1" {
		t.Fatalf("wsA re-serve (private hit): ok=%v md=%q", ok, mdA2)
	}
	if conv.calls != 2 {
		t.Fatalf("wsA re-serve must hit the private cache (no convert), conversions=%d", conv.calls)
	}
}

// TestDistill_PooledServe_AllSwitchesOn proves the consented cross-tenant serve:
// global switch on AND both producer and requester opted in → requester is
// served the producer's pooled artifact without re-converting.
func TestDistill_PooledServe_AllSwitchesOn(t *testing.T) {
	conv := &countingConv{}
	d := newScopedDistiller(t, conv, true, map[string]bool{"wsA": true, "wsB": true})
	ctx := context.Background()
	doc := docBlockBytes("shared-doc")

	mdA, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsA") // produce + publish pooled (owner=wsA)
	if !ok || mdA != "converted-1" {
		t.Fatalf("wsA produce: ok=%v md=%q", ok, mdA)
	}
	mdB, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsB") // served wsA's pooled artifact
	if !ok || mdB != "converted-1" {
		t.Fatalf("wsB should be served wsA's pooled artifact, got ok=%v md=%q", ok, mdB)
	}
	if conv.calls != 1 {
		t.Fatalf("consented cross-tenant serve must avoid re-conversion, conversions=%d", conv.calls)
	}
}

// TestDistill_PooledDenied_RequesterNotOptedIn: global on, producer opted in,
// but the requester did NOT opt in → no cross-tenant serve (fresh convert).
func TestDistill_PooledDenied_RequesterNotOptedIn(t *testing.T) {
	conv := &countingConv{}
	d := newScopedDistiller(t, conv, true, map[string]bool{"wsA": true}) // wsB absent
	ctx := context.Background()
	doc := docBlockBytes("doc")
	_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA")
	mdB, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsB")
	if !ok || mdB != "converted-2" {
		t.Fatalf("a non-opted-in requester must not be served pooled; ok=%v md=%q", ok, mdB)
	}
}

// TestDistill_PooledDenied_OwnerOptOut: the OWNER's opt-in is checked at SERVE
// time — if the producer opts out after publishing, its pooled artifact is no
// longer servable to others.
func TestDistill_PooledDenied_OwnerOptOut(t *testing.T) {
	conv := &countingConv{}
	poolable := map[string]bool{"wsA": true, "wsB": true}
	d := newScopedDistiller(t, conv, true, poolable)
	ctx := context.Background()
	doc := docBlockBytes("doc")
	_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA") // wsA publishes pooled
	poolable["wsA"] = false                           // wsA revokes its consent
	mdB, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsB")
	if !ok || mdB != "converted-2" {
		t.Fatalf("owner opt-out must deny the pooled serve; ok=%v md=%q", ok, mdB)
	}
}

// TestDistill_PooledDenied_GlobalFlagOff: both opted in but the global switch is
// off → strictly private (serve-neutral).
func TestDistill_PooledDenied_GlobalFlagOff(t *testing.T) {
	conv := &countingConv{}
	d := newScopedDistiller(t, conv, false, map[string]bool{"wsA": true, "wsB": true})
	ctx := context.Background()
	doc := docBlockBytes("doc")
	_, _, _, _ = d.tryConvertBlock(ctx, doc, nil, "wsA")
	mdB, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsB")
	if !ok || mdB == "converted-1" {
		t.Fatalf("global flag off must keep serving private; md=%q", mdB)
	}
}
