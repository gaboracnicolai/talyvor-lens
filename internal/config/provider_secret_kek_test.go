package config

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/envelope"
)

// provider_secret_kek_test.go — the BOOT half of W6.10. The posture is copied verbatim from
// talyvor-track's TRACK_INTEGRATION_ENCRYPTION_KEY, which W6.10 names as the better half of its
// pointer: UNSET ⇒ the capability is disabled with no plaintext fallback and Lens still boots;
// SET ⇒ validated to exactly 32 decoded bytes AT BOOT, so a wrong length is fail-loud at startup
// rather than a broken-crypto surprise at first use.
//
// Positive-controlled by scripts/w610-envelope-controls-h3n8.py (the K-series).

// baseEnv supplies the five variables Load refuses to boot without, so that every assertion below
// is about the KEK and not about them.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LENS_REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("LENS_DATABASE_URL", "postgres://lens:lens@localhost:5432/lens?sslmode=disable")
	t.Setenv("LENS_NATS_URL", "nats://localhost:4222")
	t.Setenv("LENS_OPENAI_API_KEY", "sk-test-openai")
	t.Setenv("LENS_ANTHROPIC_API_KEY", "sk-test-anthropic")
}

func b64Key(t *testing.T, n int) string {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

func TestK1_UnsetDisablesProviderSecretCustodyAndLensStillBoots(t *testing.T) {
	baseEnv(t)
	t.Setenv("LENS_PROVIDER_SECRET_KEK", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("[K1] Lens refused to boot with no provider-secret KEK. The posture is OPT-IN: an operator who does not want BYOK must not be forced to invent a key. err=%v", err)
	}
	if c.ProviderSecretsEnabled() {
		t.Fatalf("[K1] provider-secret custody reports ENABLED with no KEK configured — that is the plaintext-fallback shape W6.10 forbids")
	}
	if c.ProviderSecretKeyring != nil {
		t.Fatalf("[K1] a keyring exists with no KEK configured: %v", c.ProviderSecretKeyring.KeyIDs())
	}
}

func TestK2_AValidKeyArmsCustodyAndBuildsAUsableRing(t *testing.T) {
	baseEnv(t)
	t.Setenv("LENS_PROVIDER_SECRET_KEK", b64Key(t, envelope.KEKLen))

	c, err := Load()
	if err != nil {
		t.Fatalf("[K2] Load: %v", err)
	}
	if !c.ProviderSecretsEnabled() {
		t.Fatalf("[K2] a valid KEK is configured and custody still reports DISABLED — the capability would be silently inert, which is the exact shape of the mute-variable defect this repo has hit four times")
	}
	if got := c.ProviderSecretKeyring.KeyIDs(); len(got) != 1 {
		t.Fatalf("[K2] expected 1 key in the ring, got %v", got)
	}
	// And it must actually work — a ring that parses but cannot seal is not armed.
	s, err := c.ProviderSecretKeyring.Seal([]byte("sk-provider"), []byte("ws_1|anthropic"))
	if err != nil {
		t.Fatalf("[K2-USE] the configured ring cannot seal: %v", err)
	}
	if err := c.ProviderSecretKeyring.Use(s, []byte("ws_1|anthropic"), func([]byte) error { return nil }); err != nil {
		t.Fatalf("[K2-USE] the configured ring cannot open what it sealed: %v", err)
	}
}

func TestK3_AMisconfiguredKeyIsFailLoudAtBootAndNamesTheVariable(t *testing.T) {
	for _, c := range []struct{ name, val, want string }{
		{"not base64", "this is not base64!!", "LENS_PROVIDER_SECRET_KEK"},
		{"31 bytes", b64Key(t, 31), "LENS_PROVIDER_SECRET_KEK"},
		{"33 bytes", b64Key(t, 33), "LENS_PROVIDER_SECRET_KEK"},
		{"AES-128 key", b64Key(t, 16), "LENS_PROVIDER_SECRET_KEK"},
		{"whitespace only", "   ", "LENS_PROVIDER_SECRET_KEK"},
		{"same key twice", "DUP", "LENS_PROVIDER_SECRET_KEK"},
	} {
		t.Run(c.name, func(t *testing.T) {
			baseEnv(t)
			val := c.val
			if val == "DUP" {
				k := b64Key(t, envelope.KEKLen)
				val = k + "," + k
			}
			t.Setenv("LENS_PROVIDER_SECRET_KEK", val)

			cfg, err := Load()
			if err == nil {
				t.Fatalf("[K3-%s] Lens BOOTED with a misconfigured KEK (custody enabled=%v). A wrong key must die at startup, not at the first customer credential.", c.name, cfg.ProviderSecretsEnabled())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("[K3-%s] the boot error does not name %s, so the operator cannot tell which variable is wrong: %v", c.name, c.want, err)
			}
		})
	}
}

func TestK4_TheFirstListedKeyIsPrimarySoARotationTakesEffect(t *testing.T) {
	newKey, oldKey := b64Key(t, envelope.KEKLen), b64Key(t, envelope.KEKLen)
	baseEnv(t)
	t.Setenv("LENS_PROVIDER_SECRET_KEK", newKey+","+oldKey)

	c, err := Load()
	if err != nil {
		t.Fatalf("[K4] Load: %v", err)
	}
	ids := c.ProviderSecretKeyring.KeyIDs()
	if len(ids) != 2 {
		t.Fatalf("[K4] expected a 2-key ring, got %v", ids)
	}
	// The rotation instruction in the runbook is "PREPEND the new key". If the first listed key is
	// not primary, an operator who follows it keeps sealing under the key they are retiring.
	want, err := envelope.ParseKeyring(newKey)
	if err != nil {
		t.Fatalf("[K4] ParseKeyring: %v", err)
	}
	if c.ProviderSecretKeyring.PrimaryKeyID() != want.PrimaryKeyID() {
		t.Fatalf("[K4] primary is %q but the FIRST listed key is %q — prepending a new key would not rotate anything", c.ProviderSecretKeyring.PrimaryKeyID(), want.PrimaryKeyID())
	}
}

// TestK5_TheKEKIsNotReadableBackOffTheConfig is the config-layer half of W6.10's write-only rule.
// Config is dumped, logged and diffed by operators; a KEK that survives into any of those is the
// database compromise the envelope exists to prevent, with extra steps.
//
// Two independent readings, because either alone is easy to satisfy by accident: a %#v dump (what
// an operator actually prints) and a reflective walk of every exported string/[]byte field (where a
// future "keep the raw key around, it is handy" edit would land).
func TestK5_TheKEKIsNotReadableBackOffTheConfig(t *testing.T) {
	key := b64Key(t, envelope.KEKLen)
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	baseEnv(t)
	t.Setenv("LENS_PROVIDER_SECRET_KEK", key)

	c, err := Load()
	if err != nil {
		t.Fatalf("[K5] Load: %v", err)
	}

	dump := fmt.Sprintf("%#v", *c)
	if strings.Contains(dump, key) {
		t.Fatalf("[K5-B64] the base64 KEK is legible in a %%#v dump of Config — anything that logs config leaks the key")
	}
	if strings.Contains(dump, string(raw)) {
		t.Fatalf("[K5-RAW] the raw KEK bytes are legible in a %%#v dump of Config")
	}
	// FLOORS, so "not found" is a measurement rather than a short read.
	if len(dump) < 500 {
		t.Fatalf("[K5-FLOOR] the config dump is %d bytes — too short to have covered the struct", len(dump))
	}
	if !strings.Contains(dump, "ProviderSecretKeyring") {
		t.Fatalf("[K5-ANCHOR] the dump never reached the ProviderSecretKeyring field, so it could not have seen key material there either")
	}

	// The reflective half: no exported string or []byte field of Config may carry the key.
	v := reflect.ValueOf(*c)
	seen := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		var got string
		switch {
		case v.Field(i).Kind() == reflect.String:
			got = v.Field(i).String()
		case v.Field(i).Kind() == reflect.Slice && v.Field(i).Type().Elem().Kind() == reflect.Uint8:
			got = string(v.Field(i).Bytes())
		default:
			continue
		}
		seen++
		if got != "" && (strings.Contains(got, key) || strings.Contains(got, string(raw))) {
			t.Fatalf("[K5-FIELD] Config.%s carries the KEK. The keyring is the only thing that may hold it.", f.Name)
		}
	}
	if seen < 20 {
		t.Fatalf("[K5-WALKFLOOR] the reflective walk inspected only %d string/[]byte fields — it is not reading the struct, and an empty walk always looks clean", seen)
	}
}
