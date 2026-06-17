package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
	"github.com/talyvor/lens/internal/workspace"
)

// fakeEarnSink records IsOptedIn + RecordPattern calls so earnPattern can be
// tested across the flag/opt-in/auth matrix without the serve harness.
type fakeEarnSink struct {
	optedIn    bool
	optErr     error
	recErr     error
	optInCalls int
	recCalls   int
	lastWS     string
	lastReqID  string
	lastOpted  bool
}

func (f *fakeEarnSink) IsOptedIn(_ context.Context, ws string) (bool, error) {
	f.optInCalls++
	return f.optedIn, f.optErr
}
func (f *fakeEarnSink) RecordPattern(_ context.Context, ws string, _ mining.RoutingPattern, optedIn bool, requestID string) error {
	f.recCalls++
	f.lastWS, f.lastReqID, f.lastOpted = ws, requestID, optedIn
	return f.recErr
}

func earnProxy(sink patternEarnSink, enabled bool) *Proxy {
	p := &Proxy{}
	p.SetPatternEarn(sink, func() bool { return enabled })
	return p
}

// authCtx injects an authenticated principal (gate b) the way AuthMiddleware
// does on every authed path: the validated APIKey (read via GetAPIKey).
func authCtx(ws string) context.Context {
	return auth.WithAPIKey(context.Background(), &auth.APIKey{WorkspaceID: ws})
}

// workHash mirrors earnPattern's content-hash construction (digest-of-digests).
func workHash(model, prompt, response string) string {
	return poolroyalty.SHA256Hex([]byte(
		poolroyalty.SHA256Hex([]byte(model)) +
			poolroyalty.SHA256Hex([]byte(prompt)) +
			poolroyalty.SHA256Hex([]byte(response)),
	))
}

// earn(...) the standard call: scored, model "gpt-4o", prompt "p", response "r".
func callEarn(p *Proxy, ctx context.Context) bool {
	return p.earnPattern(ctx, false, false, workspace.LoggingMetadata, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50)
}

// FLAG-OFF SERVE-NEUTRALITY (the make-or-break): earn flag off → earnPattern
// returns false with ZERO sink calls (no IsOptedIn, no RecordPattern, no auth
// cost) → caller runs capturePattern.
func TestEarnPattern_FlagOff_NoCallReturnsFalse(t *testing.T) {
	s := &fakeEarnSink{optedIn: true}
	p := earnProxy(s, false)
	if p.earnPattern(authCtx("wsA"), false, false, workspace.LoggingMetadata, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50) {
		t.Fatal("flag OFF must return false (caller runs capture)")
	}
	if s.optInCalls != 0 || s.recCalls != 0 {
		t.Fatalf("flag OFF must make ZERO sink calls; IsOptedIn=%d RecordPattern=%d", s.optInCalls, s.recCalls)
	}
}

// FLAG-ON + authed + opted-in → RecordPattern called once with the CONTENT-HASH
// requestID + ac.WorkspaceID + optedIn=true; returns true (capture skipped).
func TestEarnPattern_FlagOnOptedIn_Earns(t *testing.T) {
	s := &fakeEarnSink{optedIn: true}
	p := earnProxy(s, true)
	if !callEarn(p, authCtx("wsA")) {
		t.Fatal("flag on + authed + opted-in must EARN (return true)")
	}
	if s.recCalls != 1 {
		t.Fatalf("RecordPattern must be called once; got %d", s.recCalls)
	}
	if s.lastWS != "wsA" {
		t.Errorf("must credit the AUTHENTICATED workspace (ac.WorkspaceID=wsA), got %q", s.lastWS)
	}
	if !s.lastOpted {
		t.Error("optedIn must be true")
	}
	wantRID := workHash("gpt-4o", "p", "r")
	if s.lastReqID != wantRID {
		t.Errorf("requestID must be the content hash %q, got %q", wantRID, s.lastReqID)
	}
}

// FLAG-ON, no-earn states → false (caller runs capture), no RecordPattern:
//   not-opted-in; ''/'default' workspace; nil auth context (no panic).
func TestEarnPattern_FlagOn_NoEarnStates(t *testing.T) {
	cases := []struct {
		name    string
		sink    *fakeEarnSink
		ctx     context.Context
		wantRec int
	}{
		{"not opted-in", &fakeEarnSink{optedIn: false}, authCtx("wsA"), 0},
		{"empty workspace (admin/global key)", &fakeEarnSink{optedIn: true}, authCtx(""), 0},
		{"default workspace (unauth fallback)", &fakeEarnSink{optedIn: true}, authCtx("default"), 0},
		{"nil auth context (in-process/tests)", &fakeEarnSink{optedIn: true}, context.Background(), 0},
		{"IsOptedIn read error (fail-safe)", &fakeEarnSink{optErr: errors.New("db down")}, authCtx("wsA"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := earnProxy(c.sink, true)
			if callEarn(p, c.ctx) {
				t.Fatalf("%s: must NOT earn (return false → caller runs capture)", c.name)
			}
			if c.sink.recCalls != c.wantRec {
				t.Fatalf("%s: RecordPattern calls=%d, want %d", c.name, c.sink.recCalls, c.wantRec)
			}
		})
	}
}

// ERROR SWALLOWED: RecordPattern errors → earnPattern returns TRUE (took the
// row decision; capture skipped), error swallowed, no panic.
func TestEarnPattern_RecordError_Swallowed_ReturnsTrue(t *testing.T) {
	s := &fakeEarnSink{optedIn: true, recErr: errors.New("write failed")}
	p := earnProxy(s, true)
	if !callEarn(p, authCtx("wsA")) {
		t.Fatal("a write error must still return true (earn took the row; capture must NOT double-write)")
	}
	if s.recCalls != 1 {
		t.Fatalf("RecordPattern attempted once; got %d", s.recCalls)
	}
}

// CONTENT HASH: deterministic over (model, prompt, response); not a caller header.
func TestEarnPattern_ContentHash_Deterministic(t *testing.T) {
	s1, s2 := &fakeEarnSink{optedIn: true}, &fakeEarnSink{optedIn: true}
	callEarn(earnProxy(s1, true), authCtx("wsA"))
	callEarn(earnProxy(s2, true), authCtx("wsA"))
	if s1.lastReqID != s2.lastReqID {
		t.Fatalf("identical (model,prompt,response) must hash to the same requestID; %q != %q", s1.lastReqID, s2.lastReqID)
	}
	if s1.lastReqID != workHash("gpt-4o", "p", "r") {
		t.Errorf("requestID must be SHA256Hex of the composed work product, got %q", s1.lastReqID)
	}
	// A different response (different work) → different hash.
	s3 := &fakeEarnSink{optedIn: true}
	earnProxy(s3, true).earnPattern(authCtx("wsA"), false, false, workspace.LoggingMetadata, "chat", "gpt-4o", "openai", "p", []byte("DIFFERENT"), 400, 100, 0.9, true, 50)
	if s3.lastReqID == s1.lastReqID {
		t.Error("different response must produce a different content hash")
	}
}

// Nil-safe: zero-value proxy / nil sink → false, no panic.
func TestEarnPattern_NilSafe(t *testing.T) {
	if (&Proxy{}).earnPattern(authCtx("wsA"), false, false, workspace.LoggingMetadata, "f", "m", "p", "pr", []byte("r"), 1, 1, 0.5, true, 1) {
		t.Fatal("zero-value proxy must return false")
	}
}

// ── CORPUS SENSITIVITY EXCLUSION (money-path: a sensitive request never mints) ──

// A sensitive request (PII, a fired guardrail, or logging==none) must DECLINE to
// earn: earnPattern returns false and RecordPattern is NEVER called (no mint, no
// CreditTx). The POSITIVE CONTROL in the same test proves the sink is wired — the
// byte-identical NON-sensitive request DOES earn (without it the negative
// assertions would be hollow). Each sensitive term is covered INDEPENDENTLY.
// logging==none is exercised here at the function seam even though today's
// proxy.go:1331 call site can't produce it (it sits inside the :1288
// LoggingNone-excluding block) — reaching that term is the payoff of the
// in-function guard.
func TestEarnPattern_SensitiveExcluded(t *testing.T) {
	t.Run("non-sensitive control EARNS (proves sink wired)", func(t *testing.T) {
		s := &fakeEarnSink{optedIn: true}
		p := earnProxy(s, true)
		if !p.earnPattern(authCtx("wsA"), false, false, workspace.LoggingMetadata, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50) {
			t.Fatal("non-sensitive request must EARN (positive control)")
		}
		if s.recCalls != 1 {
			t.Fatalf("non-sensitive control: RecordPattern must fire once; got %d", s.recCalls)
		}
	})

	for _, c := range []struct {
		name      string
		pii       bool
		guardrail bool
		logging   workspace.LoggingPolicy
	}{
		{"pii-only (guardrail=false, logging!=none)", true, false, workspace.LoggingMetadata},
		{"guardrail-only (pii=false, logging!=none)", false, true, workspace.LoggingFull},
		{"logging==none-only (pii=false, guardrail=false)", false, false, workspace.LoggingNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &fakeEarnSink{optedIn: true}
			p := earnProxy(s, true)
			if p.earnPattern(authCtx("wsA"), c.pii, c.guardrail, c.logging, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50) {
				t.Fatalf("%s: sensitive request must NOT earn (must return false)", c.name)
			}
			if s.recCalls != 0 {
				t.Fatalf("%s: sensitive request must NOT mint; RecordPattern calls=%d, want 0", c.name, s.recCalls)
			}
			if s.optInCalls != 0 {
				t.Fatalf("%s: must decline BEFORE the opt-in DB read; IsOptedIn calls=%d, want 0", c.name, s.optInCalls)
			}
		})
	}
}

// CONTROL FLOW — the proxy.go:1331 branch `if !p.earnPattern(...) { p.capturePattern(...) }`.
// A sensitive request must write ZERO routing_patterns rows: earnPattern declines
// (returns false), and on the resulting fall-through capturePattern's OWN guard
// also declines — so NEITHER sink fires. This proves earnPattern returning false
// does not let a sensitive row leak through capture.
func TestPatternCorpus_SensitiveWritesZeroRows(t *testing.T) {
	for _, c := range []struct {
		name      string
		pii       bool
		guardrail bool
		logging   workspace.LoggingPolicy
	}{
		{"pii-only", true, false, workspace.LoggingMetadata},
		{"guardrail-only", false, true, workspace.LoggingFull},
		{"logging-none-only", false, false, workspace.LoggingNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			earnSink := &fakeEarnSink{optedIn: true}
			capSink := &fakeCaptureSink{}
			p := &Proxy{}
			p.SetPatternEarn(earnSink, func() bool { return true })
			p.SetPatternCapture(capSink, func() bool { return true })

			// Exactly the proxy.go:1331 control flow.
			if !p.earnPattern(authCtx("wsA"), c.pii, c.guardrail, c.logging, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50) {
				p.capturePattern(context.Background(), c.pii, c.guardrail, c.logging, "wsA", "chat", "gpt-4o", "openai", 400, 100, 0.9, true, 50, false)
			}
			if earnSink.recCalls != 0 {
				t.Fatalf("%s: earn must not mint a sensitive row; RecordPattern=%d", c.name, earnSink.recCalls)
			}
			if len(capSink.pats) != 0 {
				t.Fatalf("%s: capture must not write a sensitive row on fall-through; RecordPatternObservation=%d", c.name, len(capSink.pats))
			}
		})
	}
}
