// Package stepup is B30.8's step-up authentication for the operator's compliance actions: on top of the admin key,
// a six-digit time-based code (RFC 6238 — HMAC-SHA1, 30-second steps) from the authenticator app holding
// LENS_OPERATOR_STEP_UP_SECRET. A code is good for its own step and the one either side of it, and once only — by
// anyone, on any replica (Claims) — so one seen over a shoulder or in a log opens nothing.
package stepup

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238's HMAC-SHA1: what every authenticator app computes
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	step   = 30 * time.Second
	digits = 6
	// minSecretBytes is RFC 4226's floor for the shared secret: 128 bits.
	minSecretBytes = 16
)

var (
	// ErrNotConfigured: no step-up secret is set, so no step-up action can be authorised.
	ErrNotConfigured = errors.New("stepup: no step-up secret is set (LENS_OPERATOR_STEP_UP_SECRET), so this action cannot be authorised")
	// ErrInvalid: the code is missing, malformed, wrong or out of date.
	ErrInvalid = errors.New("stepup: the step-up code is wrong or out of date")
	// ErrReplayed: someone already used this code.
	ErrReplayed = errors.New("stepup: this step-up code was already used; wait for the next one")
)

// Claims records each step whose code was used: Claim answers true the first time a step is claimed, and false ever
// after. The secret is one, so a step's code is the same for every operator: it is claimed once, whoever uses it.
type Claims interface {
	Claim(ctx context.Context, step int64, operator string) (bool, error)
}

// memClaims is Claims in this process alone: for one replica, and for tests.
type memClaims struct {
	mu   sync.Mutex
	used map[int64]bool
}

func (m *memClaims) Claim(_ context.Context, step int64, _ string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used[step] {
		return false, nil
	}
	m.used[step] = true
	return true, nil
}

// Verifier checks step-up codes against one shared secret.
type Verifier struct {
	secret []byte
	now    func() time.Time
	claims Claims
}

// New is a Verifier for the base32 secret an authenticator app was given, its used codes kept by claims — in this
// process alone when claims is nil. An empty secret is a Verifier that refuses every code with ErrNotConfigured.
func New(secret string, claims Claims) (*Verifier, error) {
	if claims == nil {
		claims = &memClaims{used: map[int64]bool{}}
	}
	v := &Verifier{now: time.Now, claims: claims}
	secret = strings.ToUpper(strings.Join(strings.Fields(secret), ""))
	if secret == "" {
		return v, nil
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return nil, fmt.Errorf("stepup: LENS_OPERATOR_STEP_UP_SECRET is not base32: %w", err)
	}
	if len(key) < minSecretBytes {
		return nil, fmt.Errorf("stepup: LENS_OPERATOR_STEP_UP_SECRET is %d bytes; it needs at least %d", len(key), minSecretBytes)
	}
	v.secret = key
	return v, nil
}

// Configured says whether a secret is set.
func (v *Verifier) Configured() bool { return len(v.secret) > 0 }

// Verify accepts code for operator once: a code for the current step or the one either side of it that nobody has
// used yet.
func (v *Verifier) Verify(ctx context.Context, operator, code string) error {
	if !v.Configured() {
		return ErrNotConfigured
	}
	code = strings.TrimSpace(code)
	if len(code) != digits || strings.Trim(code, "0123456789") != "" {
		return ErrInvalid
	}
	now := v.now().Unix() / int64(step/time.Second)
	for _, c := range []int64{now, now - 1, now + 1} {
		if !hmac.Equal([]byte(Code(v.secret, c)), []byte(code)) {
			continue
		}
		first, err := v.claims.Claim(ctx, c, operator)
		switch {
		case err != nil:
			return fmt.Errorf("stepup: record the code's use: %w", err)
		case !first:
			return ErrReplayed
		}
		return nil
	}
	return ErrInvalid
}

// Code is the code for key at step counter (RFC 4226's HOTP with RFC 6238's counter).
func Code(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, n%1_000_000)
}

// CodeAt is the code for key at t: what an authenticator app shows then.
func CodeAt(key []byte, t time.Time) string { return Code(key, t.Unix()/int64(step/time.Second)) }
