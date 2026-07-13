package proxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/lens/internal/outputverify"
)

type recordingSink struct {
	called bool
	rec    outputverify.VerdictRecord
}

func (s *recordingSink) Record(_ context.Context, r outputverify.VerdictRecord) (bool, error) {
	s.called = true
	s.rec = r
	return true, nil
}

// OFF-PATH: the verifier never blocks or alters the served response. Structurally, captureOutputVerdict
// takes NO http.ResponseWriter — it is incapable of writing to the client (the compiler enforces this), and
// it is invoked POST-FLUSH at the serve seam (after WriteHeader/Write). Behaviourally: default-off is a
// zero-cost no-op (no verification runs), and when enabled it records a verdict WITHOUT touching the
// response. Hashes only — no raw prompt/response text leaves the capture.
func TestOutputVerdict_OffPath_ZeroCost_And_Records(t *testing.T) {
	req := []byte(`{"response_format":{"type":"json_object"}}`)
	badResp := []byte(`{"choices":[{"message":{"role":"assistant","content":"not json"}}]}`)

	// (1) default-off → the sink is never called (no verification, zero added work on the serve path).
	off := &recordingSink{}
	p := &Proxy{}
	p.SetOutputVerifier(off, func() bool { return false })
	p.captureOutputVerdict(context.Background(), "ws", "m", "prov", req, badResp, "prompt", time.Unix(1_700_000, 0))
	if off.called {
		t.Error("default-off must not call the sink (no verification runs; zero cost)")
	}

	// (2) enabled → records the intrinsic verdict (a json_object request + non-JSON response → invalid_json).
	on := &recordingSink{}
	p.SetOutputVerifier(on, func() bool { return true })
	p.captureOutputVerdict(context.Background(), "ws", "m", "prov", req, badResp, "prompt", time.Unix(1_700_000, 0))
	if !on.called {
		t.Fatal("enabled: the sink must be called post-flush")
	}
	if on.rec.Verdict != outputverify.VerdictFailed || on.rec.Reason != outputverify.ReasonInvalidJSON {
		t.Errorf("want failed/invalid_json; got verdict=%q reason=%q", on.rec.Verdict, on.rec.Reason)
	}
	// Hashes only — the record carries *_sha256, never the raw prompt/response text.
	if on.rec.PromptSHA256 == "" || on.rec.ResponseSHA256 == "" ||
		strings.Contains(on.rec.PromptSHA256, "prompt") || strings.Contains(on.rec.ResponseSHA256, "not json") {
		t.Errorf("record must carry hashes only, no raw content; got %+v", on.rec)
	}
	if len(on.rec.OutputID) != 64 {
		t.Errorf("output_id must be a bound sha256 hex; got %q", on.rec.OutputID)
	}
	// HEADER == STORED: the X-Talyvor-Output-Id header the caller receives is derived by the SAME helper
	// with the SAME inputs, so it EQUALS the stored verdict row's output_id (the id the report-back keys on).
	wantID, _, _ := deriveOutputID("ws", "m", "prompt", badResp, time.Unix(1_700_000, 0))
	if on.rec.OutputID != wantID {
		t.Errorf("stored output_id must equal the header-derived id; stored=%q header=%q", on.rec.OutputID, wantID)
	}
}
