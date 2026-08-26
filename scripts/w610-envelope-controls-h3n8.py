#!/usr/bin/env python3
"""W6.10 positive controls for internal/envelope.

Every guard in envelope_test.go passed on its first run. This harness proves each one can go RED by
mutating the implementation one property at a time and requiring a NAMED marker in the failure
output. A control that merely turns the package red proves nothing — a compile error would do that
— so each case asserts the specific [Pn-...] tag of the guard it is aiming at.

Every mutation is restored and the restore is sha256-verified: this file holds the only crypto in
the repository and a control that leaves it mutated is worse than no control.
"""
import hashlib
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = ROOT / "internal" / "envelope" / "envelope.go"
TEST = ROOT / "internal" / "envelope" / "envelope_test.go"
CFG = ROOT / "internal" / "config" / "config.go"
COMPOSE = ROOT / "docker-compose.yaml"


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run_tests():
    r = subprocess.run(
        ["go", "test", "-count=1", "./internal/envelope/", "./internal/config/"],
        cwd=ROOT, capture_output=True, text=True,
    )
    return r.returncode, r.stdout + r.stderr


# (id, target file, what the mutation SIMULATES, [(old, new), ...], marker that must appear)
#
# ⚠ FOUR OF THESE FAILED ON THEIR FIRST RUN AND TWO OF THE FOUR WERE MY HARNESS, NOT THE CODE.
# Recorded rather than quietly fixed, because "the control did not fire" and "the guard is blind"
# look identical from the outside and only one of them is a finding:
#   M1  the guard WAS blind — P3-DEK named the data key and compared the WRAP NONCE. Real finding.
#   M2  my mutation redeclared err and would not compile; a control that cannot build proves the
#       compiler noticed, not the test.
#   M3  I dropped aad on ONE side, so the record simply failed to open and P4 passed for the wrong
#       reason. The realistic defect drops it on BOTH.
#   M13 pointing the census at a MISSING directory hits the parse error, not the population floor.
#       Vacuity has to be simulated with a directory that exists and holds no envelope source.
CASES = [
    ("M1", SRC, "a 'dedup' optimisation derives the DEK from the plaintext instead of randomly",
     [('\tdek := make([]byte, dekLen)\n\tif _, err := io.ReadFull(rand.Reader, dek); err != nil {\n\t\treturn Sealed{}, fmt.Errorf("envelope: generating a data key: %w", err)\n\t}',
       '\tdigest := sha256.Sum256(plaintext)\n\tdek := append([]byte(nil), digest[:]...)\n\tif false {\n\t\t_ = io.ReadFull\n\t}')],
     "[P3-DEK]"),

    ("M2", SRC, "the data nonce is forgotten and left zero — AES-GCM nonce reuse",
     [("\tctNonce, err := freshNonce(dataAEAD)\n\tif err != nil {\n\t\treturn Sealed{}, err\n\t}",
       "\tctNonce := make([]byte, dataAEAD.NonceSize())")],
     "[P3-NONCE]"),

    ("M3", SRC, "aad looks unused and is dropped from BOTH sides, so the round trip still works",
     [("\tciphertext := dataAEAD.Seal(nil, ctNonce, plaintext, aad)",
       "\tciphertext := dataAEAD.Seal(nil, ctNonce, plaintext, nil)"),
      ("\tplaintext, err := dataAEAD.Open(nil, s.CTNonce, s.Ciphertext, aad)",
       "\tplaintext, err := dataAEAD.Open(nil, s.CTNonce, s.Ciphertext, nil)")],
     "[P4]"),

    ("M4", SRC, "Rewrap is reimplemented as a re-encrypt of the stored ciphertext",
     [("\tout := s\n\tout.KeyID = primary.id\n\tout.WrappedDEK = primary.aead.Seal(nil, dekNonce, dek, dekWrapAAD)\n\tout.DEKNonce = dekNonce\n\treturn out, nil",
       "\tout := s\n\tout.KeyID = primary.id\n\tout.WrappedDEK = primary.aead.Seal(nil, dekNonce, dek, dekWrapAAD)\n\tout.DEKNonce = dekNonce\n\tout.Ciphertext = append(append([]byte(nil), 0x00), s.Ciphertext...)\n\treturn out, nil")],
     "[P5-CT]"),

    ("M5", SRC, "Rewrap takes an 'already fine' shortcut and returns the record unchanged",
     [("func (r *Keyring) Rewrap(s Sealed) (Sealed, error) {\n\tdek, err := r.unwrap(s)",
       "func (r *Keyring) Rewrap(s Sealed) (Sealed, error) {\n\tif true {\n\t\treturn s, nil\n\t}\n\tdek, err := r.unwrap(s)")],
     "[P5-DEK]"),

    ("M6", SRC, "a convenience getter is added — the write-only rule broken the obvious way",
     [("type kekEntry struct {",
       "// Plaintext is exactly the kind of convenience getter W6.10 forbids.\nfunc (s Sealed) Plaintext() []byte { return s.Ciphertext }\n\ntype kekEntry struct {")],
     "[P6]"),

    ("M7", SRC, "the wipe is turned into a no-op (or never ran)",
     [("\tsubtle.ConstantTimeCopy(1, b, make([]byte, len(b)))", "\t_ = subtle.ConstantTimeCopy")],
     "[P7]"),

    ("M8", SRC, "a dropped KEK reports as a generic decrypt failure instead of ErrUnknownKey",
     [('\t\treturn nil, fmt.Errorf("%w: key id %q (the ring holds %v)", ErrUnknownKey, s.KeyID, r.KeyIDs())',
       '\t\treturn nil, fmt.Errorf("envelope: decrypt failed")')],
     "[P8]"),

    ("M9", SRC, "the AES-256 length check is relaxed to 'any non-empty key'",
     [("\t\tif len(k) != KEKLen {", "\t\tif len(k) == 0 {")],
     "[P9-"),

    ("M10", SRC, "the duplicate-KEK check is dropped",
     [("\t\tif _, dup := r.byID[id]; dup {", "\t\tif _, dup := r.byID[id]; dup && false {")],
     "[P9b]"),

    ("M11", SRC, "ParseKeyring keeps the LAST key primary, so prepending a new key does nothing",
     [("\treturn NewKeyring(keys...)",
       "\tfor i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {\n\t\tkeys[i], keys[j] = keys[j], keys[i]\n\t}\n\treturn NewKeyring(keys...)")],
     "[P10-ORDER]"),

    ("M12", SRC, "Seal stores the plaintext and forgets to encrypt it",
     [("\treturn Sealed{\n\t\tKeyID:      primary.id,",
       "\tciphertext = append(append([]byte(nil), plaintext...), ciphertext...)\n\treturn Sealed{\n\t\tKeyID:      primary.id,")],
     "[P1-PLAIN]"),

    ("M13", SRC, "Use fails OPEN: a failed authentication hands the callback a buffer anyway",
     [('\t\treturn fmt.Errorf("envelope: the secret failed authentication (tampered ciphertext, or the wrong aad): %w", err)',
       '\t\tplaintext, err = []byte("recovered"), nil')],
     "[P2-"),

    ("M14", TEST, "VACUITY: the census reads a real directory that holds no envelope source",
     [('\tdir := "."', '\tdir := "../catalog"')],
     "[P6-POP]"),

    ("M15", TEST, "VACUITY: the census filter excludes every file, leaving an empty population",
     [('!strings.HasSuffix(name, ".go")', '!strings.HasSuffix(name, ".nonexistent")')],
     "[P6-"),

    # ── K-series: the BOOT half. Same rule — each must name the guard it aims at. ──
    ("K1", CFG, "the fail-loud boot check is dropped and a bad KEK is swallowed",
     [("\t\tring, err := envelope.ParseKeyring(v)\n\t\tif err != nil {",
       "\t\tring, err := envelope.ParseKeyring(v)\n\t\tif err != nil && false {")],
     "[K3-"),

    ("K2", CFG, "custody reports ENABLED whether or not a KEK was configured",
     [("func (c *Config) ProviderSecretsEnabled() bool { return c.ProviderSecretKeyring != nil }",
       "func (c *Config) ProviderSecretsEnabled() bool { return true }")],
     "[K1]"),

    ("K3", CFG, "custody reports DISABLED even with a valid KEK — the mute-capability shape",
     [("func (c *Config) ProviderSecretsEnabled() bool { return c.ProviderSecretKeyring != nil }",
       "func (c *Config) ProviderSecretsEnabled() bool { return false }")],
     "[K2]"),

    ("K4", CFG, "the raw KEK is kept on the Config 'because it is handy'",
     [("\tProviderSecretKeyring *envelope.Keyring",
       "\tProviderSecretKeyring *envelope.Keyring\n\tProviderSecretKEKRaw  string"),
      ("\t\tc.ProviderSecretKeyring = ring",
       "\t\tc.ProviderSecretKeyring = ring\n\t\tc.ProviderSecretKEKRaw = v")],
     "[K5-"),

    ("K5", CFG, "the KEK is never read at all — the variable is inert",
     [('\tif v := os.Getenv("LENS_PROVIDER_SECRET_KEK"); v != "" {',
       '\tif v := os.Getenv("LENS_PROVIDER_SECRET_KEK"); false {\n\t\t_ = v')],
     "[K2]"),
]


def main():
    before = {p: sha(p) for p in (SRC, TEST, CFG, COMPOSE)}

    code, out = run_tests()
    if code != 0:
        print("BASELINE IS NOT GREEN — a control run on a red package proves nothing:\n" + out)
        return 1
    print("baseline: internal/envelope + internal/config GREEN\n")

    caught, missed = [], []
    for cid, target, why, edits, marker in CASES:
        original = target.read_text()
        anchors_ok = all(original.count(o) == 1 for o, _ in edits)
        if not anchors_ok:
            counts = [original.count(o) for o, _ in edits]
            print(f"{cid} ANCHOR MISS: mutation sites occur {counts} times, want all 1 — "
                  f"the control never ran. ({why})")
            missed.append(cid)
            continue
        try:
            mutated = original
            for o, n in edits:
                mutated = mutated.replace(o, n, 1)
            target.write_text(mutated)
            code, out = run_tests()
            if code == 0:
                print(f"{cid} NOT CAUGHT — {why}: the package is STILL GREEN.")
                missed.append(cid)
            elif marker not in out:
                tags = sorted(set(re.findall(r"\[[PK]\d+[A-Za-z0-9 -]*\]", out)))
                print(f"{cid} WRONG GUARD — {why}: red, but {marker} never fired. Saw {tags}. "
                      f"Something else caught it, so the guard I am testing is unproven.")
                missed.append(cid)
            else:
                print(f"{cid} CAUGHT by {marker} — {why}")
                caught.append(cid)
        finally:
            target.write_text(original)
            if sha(target) != before[target]:
                print(f"{cid} RESTORE FAILED on {target} — STOPPING. This file is the repo's only crypto.")
                return 2

    # ── C1: the compose guard, both directions. It lives in ./cmd/lens/, so it needs its own
    # runner. ⚠ THIS CONTROL EXISTS BECAUSE THE GUARD WAS GREEN WITHOUT IT: measured 2026-08-26,
    # with LENS_PROVIDER_SECRET_KEK absent from docker-compose.yaml entirely, the guard passed —
    # mustForward is a hand-list. It only fails once the variable is added to that list by hand.
    def compose_guard():
        r = subprocess.run(["go", "test", "-count=1", "./cmd/lens/", "-run", "TestComposeForwards"],
                           cwd=ROOT, capture_output=True, text=True)
        return r.returncode, r.stdout + r.stderr

    LINE = "      - LENS_PROVIDER_SECRET_KEK=${LENS_PROVIDER_SECRET_KEK:-}\n"
    comp_orig = COMPOSE.read_text()
    if comp_orig.count(LINE) != 1:
        print(f"C1 ANCHOR MISS: the compose line occurs {comp_orig.count(LINE)} times, want 1")
        missed.append("C1")
    else:
        code_a, _ = compose_guard()
        try:
            COMPOSE.write_text(comp_orig.replace(LINE, "", 1))
            code_b, out_b = compose_guard()
        finally:
            COMPOSE.write_text(comp_orig)
        if code_a != 0:
            print("C1 BASELINE RED — the compose guard fails with the line PRESENT")
            missed.append("C1")
        elif code_b == 0:
            print("C1 NOT CAUGHT — the compose guard is GREEN with the variable absent from compose")
            missed.append("C1")
        elif "LENS_PROVIDER_SECRET_KEK" not in out_b:
            print("C1 WRONG GUARD — red, but the failure does not name the variable")
            missed.append("C1")
        else:
            print("C1 CAUGHT by TestComposeForwardsEveryCriticalConfigVar — compose line removed")
            caught.append("C1")

    code, out = run_tests()
    for f in before:
        if sha(f) != before[f]:
            print(f"RESTORE DRIFT on {f}")
            return 2
    print(f"\nrestored: sha256 matches for all {len(before)} touched files; re-run {'GREEN' if code == 0 else 'RED'}")
    print(f"CONTROLS: {len(caught)}/{len(CASES) + 1} CAUGHT" + (f"; MISSED {missed}" if missed else ""))
    return 0 if not missed and code == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
