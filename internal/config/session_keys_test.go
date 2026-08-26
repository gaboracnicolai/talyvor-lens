package config

import (
	"strings"
	"testing"
	"time"
)

// session_keys_test.go — W4.6.1 step 4.
//
// ⚠ THE DEFAULT IS THE SECURITY DECISION. Session keys are a NEW authz surface — a credential that
// mints a credential. A deployment that has not thought about it must be unchanged by the feature,
// which means OFF, which means the routes are never registered and every tlv_sk_ bearer is refused
// by the same branch that would have admitted it.

func TestLoad_SessionKeysAreOffByDefault(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionKeysEnabled {
		t.Error("SessionKeysEnabled must default FALSE — it registers routes that mint a proxy-capable " +
			"credential, and a deployment that never opted in must be byte-for-byte unchanged")
	}
	if c.SessionKeyTTL != DefaultSessionKeyTTL {
		t.Errorf("SessionKeyTTL = %v, want the documented default %v", c.SessionKeyTTL, DefaultSessionKeyTTL)
	}
}

func TestLoad_SessionKeyTTL_ParsedWhenSet(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_SESSION_KEYS_ENABLED", "true")
	t.Setenv("LENS_SESSION_KEY_TTL", "45m")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SessionKeysEnabled {
		t.Error("LENS_SESSION_KEYS_ENABLED=true did not enable the feature")
	}
	if c.SessionKeyTTL != 45*time.Minute {
		t.Errorf("SessionKeyTTL = %v, want 45m", c.SessionKeyTTL)
	}
}

// ⚠ REFUSED, NOT CLAMPED — AND THE DIFFERENCE IS THE WHOLE TEST. An operator who writes 720h, reads
// the file back as 720h, and gets 12h has a config file that lies about the running process. This
// repo keeps finding that exact class; silently clamping would be a new instance of it.
func TestLoad_SessionKeyTTLAboveTheCeilingIsRefusedNotClamped(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LENS_SESSION_KEY_TTL", "720h")
	c, err := Load()
	if err == nil {
		t.Fatalf("Load accepted a %v session-key TTL and returned %v — a ceiling applied silently is "+
			"a config file that disagrees with the process", 720*time.Hour, c.SessionKeyTTL)
	}
	if !strings.Contains(err.Error(), "LENS_SESSION_KEY_TTL") {
		t.Errorf("the refusal does not name the variable an operator has to fix: %v", err)
	}
}

func TestLoad_SessionKeyTTLMalformedIsRefused(t *testing.T) {
	for _, bad := range []string{"soon", "45", "-1h", "0"} {
		t.Run(bad, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("LENS_SESSION_KEY_TTL", bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted LENS_SESSION_KEY_TTL=%q — a typo'd lifetime that silently "+
					"becomes the default is a credential living longer than the operator believes", bad)
			}
		})
	}
}

// ⚠ THE DERIVED CREDENTIAL MUST NOT HAVE A LONGER MAXIMUM THAN THE SESSION IT DERIVES FROM. If
// MaxSessionKeyTTL ever exceeds auth.DefaultTokenTTL the exercise inverts: the "narrow, short-lived"
// key becomes the wider of the two. Restated here as a literal rather than imported because
// internal/auth imports internal/tenant and this package must stay dependency-free.
func TestSessionKeyCeilingIsShorterThanTheDefaultSessionItself(t *testing.T) {
	const authDefaultTokenTTL = 24 * time.Hour // auth.DefaultTokenTTL
	if MaxSessionKeyTTL >= authDefaultTokenTTL {
		t.Fatalf("MaxSessionKeyTTL (%v) is not shorter than auth.DefaultTokenTTL (%v) — a credential "+
			"derived from a session may not outlive the session's own default life",
			MaxSessionKeyTTL, authDefaultTokenTTL)
	}
	if DefaultSessionKeyTTL > MaxSessionKeyTTL {
		t.Fatalf("DefaultSessionKeyTTL (%v) exceeds MaxSessionKeyTTL (%v) — the default is unreachable",
			DefaultSessionKeyTTL, MaxSessionKeyTTL)
	}
}
