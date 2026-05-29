package ratelimit

import (
	"context"
	"testing"
)

type fakeShared struct {
	allow  bool
	called bool
}

func (f *fakeShared) Allow(_ context.Context, _, _ string, _ RateRule) bool {
	f.called = true
	return f.allow
}

// generous limits so the local windows always pass, isolating the shared hook.
var generousRules = []RateRule{{RequestsPerSecond: 1000, RequestsPerMinute: 1000, RequestsPerHour: 1000}}

func TestCheck_NoSharedAttached_Unchanged(t *testing.T) {
	l, _ := setupLimiter(t, generousRules)
	res := l.Check(context.Background(), "ws", "key")
	if !res.Allowed {
		t.Fatal("with generous limits and no shared checker, request should be allowed")
	}
}

func TestCheck_SharedRejects_AfterLocalWindowsPass(t *testing.T) {
	l, _ := setupLimiter(t, generousRules)
	fs := &fakeShared{allow: false}
	l.AttachShared(fs)

	res := l.Check(context.Background(), "ws", "key")
	if res.Allowed {
		t.Fatal("shared checker rejected; Check should reject even though local windows passed")
	}
	if !fs.called {
		t.Fatal("shared checker was not consulted")
	}
	if res.LimitType != "shared" {
		t.Errorf("LimitType = %q, want \"shared\"", res.LimitType)
	}
}

func TestCheck_SharedAllows_PassesThrough(t *testing.T) {
	l, _ := setupLimiter(t, generousRules)
	fs := &fakeShared{allow: true}
	l.AttachShared(fs)

	res := l.Check(context.Background(), "ws", "key")
	if !res.Allowed {
		t.Fatal("shared checker allowed; Check should allow")
	}
	if !fs.called {
		t.Fatal("shared checker was not consulted")
	}
}

func TestCheck_SharedNotConsulted_WhenLocalWindowAlreadyRejects(t *testing.T) {
	// A tight per-second limit so the local window rejects on the 2nd hit; the
	// shared checker (restrictive-only) need not be consulted in that case.
	l, _ := setupLimiter(t, []RateRule{{RequestsPerSecond: 1}})
	fs := &fakeShared{allow: true}
	l.AttachShared(fs)

	ctx := context.Background()
	_ = l.Check(ctx, "ws", "key") // 1st: allowed
	res := l.Check(ctx, "ws", "key")
	if res.Allowed {
		t.Fatal("2nd request should be rejected by the local per-second window")
	}
	if res.LimitType != "second" {
		t.Errorf("LimitType = %q, want \"second\" (local window, not shared)", res.LimitType)
	}
}
