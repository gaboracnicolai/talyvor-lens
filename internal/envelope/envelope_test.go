package envelope

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envelope_test.go — the properties W6.10 asks for, each stated so that removing the behaviour it
// names turns THIS test red. Every one is positive-controlled by
// scripts/w610-envelope-controls-h3n8.py, which mutates the implementation one property at a time.

func mustKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KEKLen)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func mustRing(t *testing.T, keys ...[]byte) *Keyring {
	t.Helper()
	r, err := NewKeyring(keys...)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	return r
}

// use is the only way a test can see a plaintext, because it is the only way ANYTHING can.
func use(t *testing.T, r *Keyring, s Sealed, aad []byte) ([]byte, error) {
	t.Helper()
	var got []byte
	err := r.Use(s, aad, func(pt []byte) error {
		got = append([]byte(nil), pt...)
		return nil
	})
	return got, err
}

// ── P1 round trip ────────────────────────────────────────────────────────────────────────────

func TestP1_SealThenUseReturnsTheExactPlaintext(t *testing.T) {
	r := mustRing(t, mustKEK(t))
	secret := []byte("sk-ant-api03-CUSTOMER-PROVIDER-CREDENTIAL")
	aad := []byte("ws_42|anthropic")

	s, err := r.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(s.Ciphertext, secret) {
		t.Fatalf("[P1-PLAIN] the sealed ciphertext CONTAINS the plaintext — nothing was encrypted")
	}
	got, err := use(t, r, s, aad)
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("[P1] round trip lost the secret: got %q want %q", got, secret)
	}
}

// ── P2 tamper detection on EVERY component ──────────────────────────────────────────────────

func TestP2_TamperingAnyComponentFailsClosed(t *testing.T) {
	r := mustRing(t, mustKEK(t))
	secret := []byte("sk-live-provider-secret")
	aad := []byte("ws_42|openai")

	// Naming every component explicitly is the point: a guard that only flips the ciphertext is
	// blind to a swapped wrapped-DEK, which is the component envelope encryption ADDS.
	for _, c := range []struct {
		name string
		bend func(*Sealed)
	}{
		{"Ciphertext", func(s *Sealed) { s.Ciphertext[0] ^= 0x01 }},
		{"CTNonce", func(s *Sealed) { s.CTNonce[0] ^= 0x01 }},
		{"WrappedDEK", func(s *Sealed) { s.WrappedDEK[0] ^= 0x01 }},
		{"DEKNonce", func(s *Sealed) { s.DEKNonce[0] ^= 0x01 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := r.Seal(secret, aad)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			c.bend(&s)
			got, err := use(t, r, s, aad)
			if err == nil {
				t.Fatalf("[P2-%s] a tampered %s OPENED — Use returned no error", c.name, c.name)
			}
			if got != nil {
				t.Fatalf("[P2-%s] Use handed a plaintext to the callback AND errored: %q", c.name, got)
			}
		})
	}
}

// ── P3 a fresh DEK per seal ─────────────────────────────────────────────────────────────────

func TestP3_EverySealGetsItsOwnDEKAndNonce(t *testing.T) {
	r := mustRing(t, mustKEK(t))
	secret := []byte("the same secret, twice")
	aad := []byte("ws_42|anthropic")

	a, err := r.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal a: %v", err)
	}
	b, err := r.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal b: %v", err)
	}
	if bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatalf("[P3-CT] two seals of the SAME plaintext produced identical ciphertext — deterministic encryption leaks equality between rows")
	}
	// ⚠ THIS ASSERTION USED TO READ bytes.Equal(a.WrappedDEK, b.WrappedDEK) AND COULD NOT FAIL.
	// Two wraps of the SAME DEK differ anyway, because the wrap nonce is fresh each time — so the
	// assertion named the data key and measured the nonce. Control M1 (a DEK derived from the
	// plaintext, the "dedup" optimisation) left the package fully GREEN. The DEKs have to be
	// compared directly, which this in-package test can do and no caller can.
	dekA, err := r.unwrap(a)
	if err != nil {
		t.Fatalf("[P3-DEK] unwrap a: %v", err)
	}
	dekB, err := r.unwrap(b)
	if err != nil {
		t.Fatalf("[P3-DEK] unwrap b: %v", err)
	}
	if bytes.Equal(dekA, dekB) {
		t.Fatalf("[P3-DEK] two seals of the same plaintext share a DATA KEY — the DEK is not per-secret, which is the property that makes this an ENVELOPE rather than one key with extra steps")
	}
	if bytes.Equal(a.WrappedDEK, b.WrappedDEK) {
		t.Fatalf("[P3-WRAP] two wrapped DEKs are byte-identical — the wrap nonce is being reused")
	}
	if bytes.Equal(a.CTNonce, b.CTNonce) {
		t.Fatalf("[P3-NONCE] nonce reuse under one key: AES-GCM loses confidentiality AND authenticity on a repeated nonce")
	}
}

// ── P4 AAD binds a row to its owner ─────────────────────────────────────────────────────────

func TestP4_ASealedRowCannotBeMovedToAnotherWorkspace(t *testing.T) {
	r := mustRing(t, mustKEK(t))
	s, err := r.Seal([]byte("workspace A's provider key"), []byte("ws_A|anthropic"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The attack this stops: copy the ciphertext column of workspace A's row into workspace B's
	// row. Without AAD binding, B's gateway then bills A's provider account.
	got, err := use(t, r, s, []byte("ws_B|anthropic"))
	if err == nil {
		t.Fatalf("[P4] a row sealed for ws_A OPENED under ws_B — the ciphertext is not bound to its owner")
	}
	if got != nil {
		t.Fatalf("[P4] plaintext leaked to the callback on an AAD mismatch: %q", got)
	}
}

// ── P5 THE ENVELOPE PROPERTY: rotation does not re-encrypt the secret ────────────────────────

func TestP5_RewrapChangesTheKeyWithoutTouchingTheCiphertext(t *testing.T) {
	oldKEK, newKEK := mustKEK(t), mustKEK(t)
	before := mustRing(t, oldKEK)
	secret := []byte("sk-provider-credential-under-rotation")
	aad := []byte("ws_42|anthropic")

	s, err := before.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	ctCopy := append([]byte(nil), s.Ciphertext...)
	ctNonceCopy := append([]byte(nil), s.CTNonce...)

	// Rotation posture: the NEW key is primary, the OLD one stays in the ring for opening.
	after := mustRing(t, newKEK, oldKEK)
	rw, err := after.Rewrap(s)
	if err != nil {
		t.Fatalf("Rewrap: %v", err)
	}

	// This is the whole reason W6.10's title says ENVELOPE and not "mirror Track". A single-key
	// design cannot rotate without rewriting every ciphertext in the table.
	if !bytes.Equal(rw.Ciphertext, ctCopy) {
		t.Fatalf("[P5-CT] rewrap REWROTE the ciphertext — this is not envelope encryption, it is a re-encrypt, and rotating a large table would rewrite every row")
	}
	if !bytes.Equal(rw.CTNonce, ctNonceCopy) {
		t.Fatalf("[P5-CTNONCE] rewrap changed the data nonce — same objection as [P5-CT]")
	}
	if bytes.Equal(rw.WrappedDEK, s.WrappedDEK) {
		t.Fatalf("[P5-DEK] rewrap left the wrapped DEK byte-identical — the old KEK still opens this row, so nothing was rotated")
	}
	if rw.KeyID == s.KeyID {
		t.Fatalf("[P5-ID] rewrap left KeyID %q — an operator cannot tell which rows have moved", rw.KeyID)
	}
	if rw.KeyID != after.PrimaryKeyID() {
		t.Fatalf("[P5-PRIMARY] rewrap landed on %q, not the primary %q", rw.KeyID, after.PrimaryKeyID())
	}

	// And the point of the exercise: it still opens, under the NEW primary alone.
	onlyNew := mustRing(t, newKEK)
	got, err := use(t, onlyNew, rw, aad)
	if err != nil {
		t.Fatalf("[P5-OPEN] a rewrapped row does not open under the new primary alone: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("[P5-OPEN] rewrap corrupted the secret: got %q", got)
	}
}

// ── P6 the write-only API shape ─────────────────────────────────────────────────────────────

// TestP6_NoExportedFunctionHandsBackAPlaintext reads this package's own source and fails if any
// exported function or method returns a []byte, a string, or an any. W6.10's rule is "the plaintext
// must never be readable back through ANY API, by anyone, including an operator or the global admin
// key" — the only enforcement that survives a future edit is a census over the exported surface,
// because a reviewer cannot be a guard.
func TestP6_NoExportedFunctionHandsBackAPlaintext(t *testing.T) {
	fset := token.NewFileSet()
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("[P6-PARSE] the census could not read its own directory, so it asserted nothing: %v", err)
	}

	// allowed names whose []byte/string return is structurally NOT a plaintext, each with why.
	allowed := map[string]string{
		"PrimaryKeyID": "returns a KEY ID (a hash prefix), never key material",
		"KeyIDs":       "returns the key IDs in the ring, never key material",
	}

	var exported, offenders []string
	pkgFiles := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("[P6-PARSE] %s: %v", name, err)
		}
		if f.Name.Name != "envelope" {
			continue // a file from some other package cannot be part of this package's surface
		}
		pkgFiles++
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			name := fn.Name.Name
			exported = append(exported, name)
			if fn.Type.Results == nil {
				continue
			}
			for _, res := range fn.Type.Results.List {
				if !leaksBytes(res.Type) {
					continue
				}
				if _, ok := allowed[name]; ok {
					continue
				}
				offenders = append(offenders, name)
			}
		}
	}
	// FLOORS, in both directions, so this census cannot pass by reading nothing.
	if pkgFiles == 0 {
		t.Fatalf("[P6-POP] the census read %d directory entries and found NO file in package envelope — an empty population always looks clean", len(entries))
	}
	if len(exported) < 6 {
		t.Fatalf("[P6-FLOOR] the census found only %d exported functions (%v) — it is reading the wrong files, and an empty population always looks clean", len(exported), exported)
	}
	foundUse := false
	for _, n := range exported {
		if n == "Use" {
			foundUse = true
		}
	}
	if !foundUse {
		t.Fatalf("[P6-ANCHOR] the census did not see Use — if the ONE sanctioned plaintext path is invisible to it, so is an unsanctioned one. Exported: %v", exported)
	}
	for _, o := range offenders {
		t.Errorf("[P6] exported %s returns a byte/string/any to its caller. The plaintext must leave this package ONLY through Use's callback; if %s is genuinely not key material, add it to the allowed map WITH a reason.", o, o)
	}
}

// leaksBytes reports whether a return type could carry a plaintext out of the package.
func leaksBytes(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.ArrayType:
		id, ok := t.Elt.(*ast.Ident)
		return ok && id.Name == "byte" && t.Len == nil
	case *ast.Ident:
		return t.Name == "string" || t.Name == "any"
	case *ast.InterfaceType:
		return len(t.Methods.List) == 0
	}
	return false
}

// ── P7 the plaintext buffer does not outlive the callback ───────────────────────────────────

func TestP7_TheCallbackBufferIsZeroedAfterUse(t *testing.T) {
	r := mustRing(t, mustKEK(t))
	secret := []byte("sk-must-not-linger")
	aad := []byte("ws_42|openai")
	s, err := r.Seal(secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	var escaped []byte // the SAME backing array the callback saw, not a copy
	if err := r.Use(s, aad, func(pt []byte) error {
		escaped = pt
		if !bytes.Equal(pt, secret) {
			t.Fatalf("callback got the wrong plaintext")
		}
		return nil
	}); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if bytes.Equal(escaped, secret) {
		t.Fatalf("[P7] the plaintext is still legible in the callback's buffer after Use returned: %q. A caller that squirrels away the slice keeps a live credential.", escaped)
	}
	for i, b := range escaped {
		if b != 0 {
			t.Fatalf("[P7] byte %d of the callback buffer is %#x, not zero — the wipe is partial", i, b)
		}
	}
}

// ── P8 key loss is NAMED, not a generic auth failure ────────────────────────────────────────

func TestP8_ADroppedKEKIsReportedAsSuchAndNotAsCorruption(t *testing.T) {
	oldKEK := mustKEK(t)
	s, err := mustRing(t, oldKEK).Seal([]byte("secret"), []byte("ws_42|openai"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// The operator rotated and dropped the old KEK while rows still referenced it. W6.10 asks what
	// the operator SEES. If this is a generic decrypt failure they will chase data corruption.
	stranded := mustRing(t, mustKEK(t))
	err = stranded.Use(s, []byte("ws_42|openai"), func([]byte) error { return nil })
	if err == nil {
		t.Fatalf("[P8] a row sealed under a KEK that is no longer in the ring OPENED")
	}
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("[P8] a dropped KEK reports %v, which does not match ErrUnknownKey — the operator is told 'decrypt failed' and cannot tell key loss from corruption", err)
	}
	if !strings.Contains(err.Error(), s.KeyID) {
		t.Fatalf("[P8-ID] the error does not name the missing key id %q, so the operator cannot tell WHICH key to restore: %v", s.KeyID, err)
	}
}

// ── P9 fail-loud on a misconfigured KEK ─────────────────────────────────────────────────────

func TestP9_NewKeyringRefusesAnythingThatIsNotA32ByteKey(t *testing.T) {
	for _, c := range []struct {
		name string
		key  []byte
	}{
		{"empty", nil},
		{"31 bytes", make([]byte, 31)},
		{"33 bytes", make([]byte, 33)},
		{"16 bytes (AES-128)", make([]byte, 16)},
	} {
		if _, err := NewKeyring(c.key); err == nil {
			t.Errorf("[P9-%s] NewKeyring ACCEPTED a %d-byte key — AES-256 needs exactly %d", c.name, len(c.key), KEKLen)
		}
	}
	if _, err := NewKeyring(); err == nil {
		t.Errorf("[P9-none] NewKeyring with NO keys returned a usable ring — an empty ring seals under nothing")
	}
	// and the positive direction, so the four above are not passing because NewKeyring never works
	if _, err := NewKeyring(make([]byte, KEKLen)); err != nil {
		t.Fatalf("[P9-CONTROL] NewKeyring rejected a correct %d-byte key (%v) — the rejections above prove nothing", KEKLen, err)
	}
}

func TestP9b_DuplicateKeysInARingAreRefused(t *testing.T) {
	k := mustKEK(t)
	if _, err := NewKeyring(k, k); err == nil {
		t.Fatalf("[P9b] a ring accepted the same KEK twice — the decrypt-only slot is then dead weight and an operator believes they have rotated when they have not")
	}
}

// ── P10 the base64 wire form an operator actually types ─────────────────────────────────────

func TestP10_ParseKeyringReadsTheOperatorsCommaSeparatedBase64(t *testing.T) {
	k1, k2 := mustKEK(t), mustKEK(t)
	enc := base64.StdEncoding.EncodeToString(k1) + " , " + base64.StdEncoding.EncodeToString(k2)

	r, err := ParseKeyring(enc)
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	if got := r.KeyIDs(); len(got) != 2 {
		t.Fatalf("[P10] expected 2 keys in the ring, got %v", got)
	}
	direct := mustRing(t, k1, k2)
	if r.PrimaryKeyID() != direct.PrimaryKeyID() {
		t.Fatalf("[P10-ORDER] ParseKeyring made %q primary, not the FIRST listed key %q — an operator who prepends a new key would keep sealing under the old one", r.PrimaryKeyID(), direct.PrimaryKeyID())
	}
	for _, bad := range []string{"", "not-base64!!", base64.StdEncoding.EncodeToString(make([]byte, 31)), ","} {
		if _, err := ParseKeyring(bad); err == nil {
			t.Errorf("[P10-BAD] ParseKeyring accepted %q", bad)
		}
	}
}
