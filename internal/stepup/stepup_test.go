package stepup

import (
	"context"
	"encoding/base32"
	"errors"
	"testing"
	"time"
)

// RFC 6238's SHA-1 test vectors, as six digits: what an authenticator app shows for its test secret.
func TestCodeMatchesRFC6238(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if got := CodeAt(key, time.Unix(unix, 0)); got != want {
			t.Errorf("code at %d = %s; want %s", unix, got, want)
		}
	}
}

// A code is good for its own step and the ones either side, once, whoever sends it; a wrong one, an old one or an
// unset secret opens nothing.
func TestVerifyAcceptsACurrentCodeOnce(t *testing.T) {
	key := []byte("0123456789abcdef0123")
	v, err := New(base32.StdEncoding.EncodeToString(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	v.now = func() time.Time { return now }

	if err := v.Verify(ctx, "nicolai", CodeAt(key, now.Add(-30*time.Second))); err != nil {
		t.Fatalf("the previous step's code: %v", err)
	}
	if err := v.Verify(ctx, "nicolai", CodeAt(key, now)); err != nil {
		t.Fatalf("the current code: %v", err)
	}
	if err := v.Verify(ctx, "nicolai", CodeAt(key, now)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("the same code again = %v; want ErrReplayed", err)
	}
	if err := v.Verify(ctx, "ada", CodeAt(key, now)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("the same code under another operator's name = %v; want ErrReplayed", err)
	}
	for _, bad := range []string{"", "12345", "abcdef", CodeAt(key, now.Add(-2*time.Minute))} {
		if err := v.Verify(ctx, "grace", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("code %q = %v; want ErrInvalid", bad, err)
		}
	}

	unset, err := New("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unset.Verify(ctx, "nicolai", CodeAt(key, time.Now())); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("no secret = %v; want ErrNotConfigured", err)
	}
	if _, err := New("JBSWY3DPEHPK3PXP", nil); err == nil {
		t.Fatal("a 10-byte secret was taken; it needs 16")
	}
}
