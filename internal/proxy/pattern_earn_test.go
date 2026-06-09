package proxy

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/mining"
	"github.com/talyvor/lens/internal/poolroyalty"
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
	return p.earnPattern(ctx, "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50)
}

// FLAG-OFF SERVE-NEUTRALITY (the make-or-break): earn flag off → earnPattern
// returns false with ZERO sink calls (no IsOptedIn, no RecordPattern, no auth
// cost) → caller runs capturePattern.
func TestEarnPattern_FlagOff_NoCallReturnsFalse(t *testing.T) {
	s := &fakeEarnSink{optedIn: true}
	p := earnProxy(s, false)
	if p.earnPattern(authCtx("wsA"), "chat", "gpt-4o", "openai", "p", []byte("r"), 400, 100, 0.9, true, 50) {
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
	earnProxy(s3, true).earnPattern(authCtx("wsA"), "chat", "gpt-4o", "openai", "p", []byte("DIFFERENT"), 400, 100, 0.9, true, 50)
	if s3.lastReqID == s1.lastReqID {
		t.Error("different response must produce a different content hash")
	}
}

// Nil-safe: zero-value proxy / nil sink → false, no panic.
func TestEarnPattern_NilSafe(t *testing.T) {
	if (&Proxy{}).earnPattern(authCtx("wsA"), "f", "m", "p", "pr", []byte("r"), 1, 1, 0.5, true, 1) {
		t.Fatal("zero-value proxy must return false")
	}
}
