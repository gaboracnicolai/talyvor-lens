package proxy

import (
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/workspace"
)

// THE SEAM THIS WAS MEASURED AT, PINNED AT THAT SEAM.
//
// compressor.TestCompress_AForgedPlaceholderLeavesThePromptByteIdentical pins the
// refusal as a property of the package. This file pins the only thing a customer
// can observe: what the PROVIDER received. The two are not the same assertion —
// the proxy re-marshals the rewritten prompt into a provider body (rebuildBody),
// and a unit test on Compress cannot see whether the bytes it returned are the
// bytes that left the process.
//
// ⚠ THIS IS HOW THE DEFECT WAS FOUND, IN THIS ORDER: a probe through this helper
// showed the upstream capturing
//
//	"before <NUL>CODE0<NUL> after\n```python\nx = 1\n\ny = 2\n```\n"   (sent by the caller)
//	"before ```python\nx = 1\ny = 2\n``` after\n<NUL>CODE0<NUL>"       (received by the provider)
//
// with a 200 response and a recorded two-byte saving. Both halves of that were
// wrong: the fenced block moved ahead of the word that followed it, and the
// gateway's internal marker was transmitted to a third party. The compressor now
// refuses the prompt outright, so the provider receives exactly what the caller
// sent.
//
// Positive control: C20/C21 of w61-placeholder-controls-4f2b.py remove the refusal
// and both assertions below go red together.
func TestCompressWire_AForgedPlaceholderReachesTheProviderUnchanged(t *testing.T) {
	p, up, _ := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-forge", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	const forged = "before \x00CODE0\x00 after\n```python\nx = 1\n\ny = 2\n```\n"

	w := dispatchCompress(t, p, "ws-forge", forged, nil)
	if w.Code != 200 {
		t.Fatalf("premise: the request must have served; got %d", w.Code)
	}

	got := up.lastPrompt(t)
	if got != forged {
		t.Errorf("the provider received a rewritten prompt.\n got %q\nwant %q", got, forged)
	}
	// Stated separately, because "unchanged" and "our marker did not travel" fail
	// for different reasons and a single equality would report only the first.
	if strings.Count(got, "\x00CODE") != strings.Count(forged, "\x00CODE") {
		t.Errorf("the gateway's placeholder count changed on the wire: got %d, caller sent %d",
			strings.Count(got, "\x00CODE"), strings.Count(forged, "\x00CODE"))
	}
}

// THE DISCRIMINATION, AND IT IS NOT DECORATION. The refusal must be keyed on the
// encoding, not on the content: an ordinary compressible prompt through the SAME
// gate must still be rewritten. Without this, deleting the whole compressor would
// pass the test above — a refusal that refuses everything is not a fix, it is an
// outage with a passing suite.
func TestCompressWire_AnOrdinaryPromptIsStillRewritten(t *testing.T) {
	p, up, _ := newCompressionProxyWithUpstream(t, workspace.Workspace{
		ID: "ws-ordinary", Name: "always on", Active: true,
		LoggingPolicy: workspace.LoggingMetadata, CompressionPolicy: workspace.CompressionAlways,
	}, measuredUsageResponse)

	w := dispatchCompress(t, p, "ws-ordinary", compressiblePrompt, nil)
	if w.Code != 200 {
		t.Fatalf("premise: the request must have served; got %d", w.Code)
	}
	if up.lastPrompt(t) == compressiblePrompt {
		t.Error("the gate was open on a compressible prompt and nothing was rewritten — " +
			"the NUL refusal has widened into a kill switch")
	}
}
