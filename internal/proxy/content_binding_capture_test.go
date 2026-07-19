package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/outputverify"
)

// pinEnvelope is the FIXED served body for the identity lock below. The text deliberately lacks a trailing
// newline so the canonical form (text + "\n") differs from the raw text — proving the content hash is over
// canonical bytes while the identity hash stays over the RAW envelope bytes.
var pinEnvelope = []byte(`{"id":"msg_pin","type":"message","role":"assistant","content":[{"type":"text","text":"package main\n\nfunc main() {\n\tprintln(\"pin\")\n}"}],"usage":{"input_tokens":1,"output_tokens":2}}`)

// THE IDENTITY INVARIANT (pinned-value regression lock). These literals were computed at c70bb71 — the
// commit BEFORE the content binding existed — by running deriveOutputID on exactly these inputs. If
// response_sha256 or output_id ever drifts for the same served bytes, every existing verdict, attribution,
// bond, and mechanical report silently unkeys. response_sha256 and DeriveOutputID are IDENTITY: the content
// binding ADDS a column and must never redefine them.
func TestOutputIdentity_PinnedValues_ByteIdenticalToPreChange(t *testing.T) {
	const (
		wantPromptSHA   = "148de9c5a7a44d19e56cd9ae1a554bf67847afb0c58f6e12fa29ac7ddfca9940"
		wantResponseSHA = "7051ca21dd974d8b68be0575951a8c87f4702ae9e1881c99c10f8568a60dcf43"
		wantOutputID    = "d7a9e2ed4686ec5a34ae653aa226cb9828a5bc2cf96a899e3128f7275e59216d"
	)
	id, promptHash, responseHash := deriveOutputID("ws_pin", "claude-x", "p", pinEnvelope, time.Unix(1752900000, 0))
	if promptHash != wantPromptSHA {
		t.Errorf("prompt_sha256 drifted from the pre-change value:\n got %s\nwant %s", promptHash, wantPromptSHA)
	}
	if responseHash != wantResponseSHA {
		t.Errorf("response_sha256 drifted from the pre-change value (IDENTITY — must hash the RAW envelope):\n got %s\nwant %s", responseHash, wantResponseSHA)
	}
	if id != wantOutputID {
		t.Errorf("output_id drifted from the pre-change value:\n got %s\nwant %s", id, wantOutputID)
	}
}

// The capture seam computes the CONTENT hash alongside the identity hashes: same served bytes, same record —
// response_sha256 = raw envelope hash (identity), output_content_sha256 = canonical content hash (binding).
func TestCaptureOutputVerdict_RecordsContentHash(t *testing.T) {
	on := &recordingSink{}
	p := &Proxy{}
	p.SetOutputVerifier(on, func() bool { return true })
	p.captureOutputVerdict(context.Background(), "ws_pin", "claude-x", "anthropic",
		[]byte(`{}`), pinEnvelope, "p", time.Unix(1752900000, 0))
	if !on.called {
		t.Fatal("enabled: the sink must be called")
	}

	wantContent, ok := outputverify.CanonicalContentSHA256("anthropic", pinEnvelope)
	if !ok {
		t.Fatal("pin envelope must canonicalize")
	}
	if on.rec.OutputContentSHA256 != wantContent {
		t.Errorf("output_content_sha256 = %q, want the canonical content hash %q", on.rec.OutputContentSHA256, wantContent)
	}
	// And it is DISTINCT from the identity hash — content ≠ envelope.
	if on.rec.OutputContentSHA256 == on.rec.ResponseSHA256 {
		t.Error("content hash must differ from the envelope hash (they bind different bytes)")
	}
}

// A body with no committable content (e.g. raw SSE bytes that were never a JSON envelope) records the
// verdict as before with output_content_sha256 EMPTY (stored NULL) — capture behavior otherwise unchanged.
func TestCaptureOutputVerdict_NoContent_RecordsEmpty(t *testing.T) {
	on := &recordingSink{}
	p := &Proxy{}
	p.SetOutputVerifier(on, func() bool { return true })
	sse := []byte("event: message_start\ndata: {}\n\n")
	p.captureOutputVerdict(context.Background(), "ws", "m", "anthropic", []byte(`{}`), sse, "p", time.Unix(1_700_000, 0))
	if !on.called {
		t.Fatal("the verdict must still be recorded (capture unchanged)")
	}
	if on.rec.OutputContentSHA256 != "" {
		t.Errorf("no committable content must record empty (NULL); got %q", on.rec.OutputContentSHA256)
	}
}
