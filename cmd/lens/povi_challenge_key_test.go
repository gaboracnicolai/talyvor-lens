package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A MISCONFIGURED KEY MUST NOT LOOK LIKE NO KEY.
//
// LENS_POVI_CHALLENGE_KEY exists so Lens's challenge-signing pubkey survives a restart; nodes pin it,
// and an ephemeral key makes honest nodes fail challenges. The old code, on a key that was SET but
// undecodable, warned and generated an ephemeral one — so a typo produced exactly the outcome the
// variable exists to prevent, distinguishable from an unset variable only by one WARN line that an
// operator who believes the key IS configured has no reason to go looking for.
//
// LENS_JWT_PRIVATE_KEY had already settled this: present-and-unparseable exits. These tests pin the
// same asymmetry here, so the two long-lived keys cannot drift apart again.

// loadOrGenChallengeKey calls os.Exit on the invalid branch, so that branch is exercised in a
// subprocess. The parent asserts the exit CODE, not log text — a message can be reworded, and an
// assertion on prose would go green against a build that logged loudly and carried on regardless.
func TestChallengeKey_SetButInvalid_RefusesToStart(t *testing.T) {
	for _, bad := range []struct{ name, val string }{
		{"not base64 at all", "this is not base64!!"},
		{"valid base64, wrong length", base64.StdEncoding.EncodeToString([]byte("too short"))},
		{"a PEM someone pasted by mistake", "-----BEGIN PRIVATE KEY-----\nMC4CAQ==\n-----END PRIVATE KEY-----"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestChallengeKey_SubprocessBody")
			cmd.Env = append(os.Environ(), "LENS_POVI_KEY_CRASHER=1", "LENS_POVI_KEY_VALUE="+bad.val)
			out, err := cmd.CombinedOutput()

			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("a key that is set and undecodable did NOT stop startup (err=%v).\n"+
					"Falling back to an ephemeral key here invalidates every pinned node pubkey on this "+
					"restart, which is the failure LENS_POVI_CHALLENGE_KEY exists to prevent.\noutput:\n%s", err, out)
			}
			if got := ee.ExitCode(); got != 1 {
				t.Errorf("exit code = %d, want 1", got)
			}
			// The operator must be able to tell a truncated paste from a wrong format without the key
			// itself appearing anywhere.
			if !strings.Contains(string(out), "decoded_bytes") {
				t.Errorf("failure does not report decoded_bytes, so a truncated paste and a non-base64 "+
					"value are indistinguishable:\n%s", out)
			}
			if strings.Contains(string(out), bad.val) && bad.val != "" {
				t.Errorf("the rejected key VALUE was echoed into the log:\n%s", out)
			}
		})
	}
}

func TestChallengeKey_SubprocessBody(t *testing.T) {
	if os.Getenv("LENS_POVI_KEY_CRASHER") != "1" {
		t.Skip("subprocess body; driven by TestChallengeKey_SetButInvalid_RefusesToStart")
	}
	loadOrGenChallengeKey(os.Getenv("LENS_POVI_KEY_VALUE"), slog.New(slog.NewJSONHandler(os.Stderr, nil)))
}

// UNSET must still boot — a dev run has no key, and this is the branch the JWT block also keeps.
// Without this, "fail hard on a bad key" would be indistinguishable from "require a key".
func TestChallengeKey_Unset_StillGeneratesEphemeral(t *testing.T) {
	pub, priv := loadOrGenChallengeKey("", slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if len(pub) != ed25519.PublicKeySize || len(priv) != ed25519.PrivateKeySize {
		t.Fatalf("unset key did not yield a usable ephemeral keypair (pub=%d priv=%d)", len(pub), len(priv))
	}
}

// A VALID key must round-trip to a STABLE keypair — otherwise "fail hard on invalid" could be
// satisfied by rejecting everything, which would take PoVI out entirely.
func TestChallengeKey_Valid_IsStableAcrossCalls(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	b64 := base64.StdEncoding.EncodeToString(seed)
	lg := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	pub1, _ := loadOrGenChallengeKey(b64, lg)
	pub2, _ := loadOrGenChallengeKey(b64, lg)
	if !pub1.Equal(pub2) {
		t.Fatal("the same seed produced two different pubkeys — nodes pinning one would fail against the other")
	}
	// And the 64-byte private-key form must work too, since the doc offers both.
	full := ed25519.NewKeyFromSeed(seed)
	pub3, _ := loadOrGenChallengeKey(base64.StdEncoding.EncodeToString(full), lg)
	if !pub1.Equal(pub3) {
		t.Fatal("seed form and private-key form of the same key produced different pubkeys")
	}
}
