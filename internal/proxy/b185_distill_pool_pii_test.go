package proxy

import (
	"context"
	"testing"
)

// B18.5 — A SHARED DOCUMENT CONVERSION NEEDS PERSONAL-DATA DETECTION TOO.
//
// B15.7 kept the answer pool closed to a workspace whose requests are not checked for personal data;
// the distill pool still took its conversions. With wsA's detection off, wsB — opted in, sending the
// same document — converts it fresh rather than being served wsA's conversion; wsA's own cache keeps
// working. Turning detection back on shares the next conversion, which also proves the harness pools.
func TestB185_DetectionOffNeverSharesAConversion_TurningItBackOnResumes(t *testing.T) {
	conv := &countingConv{}
	d := newScopedDistiller(t, conv, true, map[string]bool{"wsA": true, "wsB": true})
	detects := map[string]bool{"wsA": false, "wsB": true}
	d.detectsPII = func(ws string) bool { return detects[ws] }
	ctx := context.Background()

	doc := docBlockBytes("a document from wsA while its detection is off")
	if md, _, _, ok := d.tryConvertBlock(ctx, doc, nil, "wsA"); !ok || md != "converted-1" {
		t.Fatalf("wsA convert: ok=%v md=%q", ok, md)
	}
	if md, _, _, _ := d.tryConvertBlock(ctx, doc, nil, "wsA"); md != "converted-1" || conv.calls != 1 {
		t.Fatalf("wsA's own cache must still serve its repeat: md=%q conversions=%d", md, conv.calls)
	}
	if md, _, _, _ := d.tryConvertBlock(ctx, doc, nil, "wsB"); md == "converted-1" {
		t.Fatal("wsB was served a conversion shared by wsA with personal-data detection off")
	}

	detects["wsA"] = true
	doc2 := docBlockBytes("a document from wsA after detection is back on")
	mdA, _, _, _ := d.tryConvertBlock(ctx, doc2, nil, "wsA")
	calls := conv.calls
	if mdB, _, _, _ := d.tryConvertBlock(ctx, doc2, nil, "wsB"); mdB != mdA || conv.calls != calls {
		t.Errorf("with detection back on, wsB must be served wsA's conversion: got %q want %q, conversions %d→%d",
			mdB, mdA, calls, conv.calls)
	}
}
