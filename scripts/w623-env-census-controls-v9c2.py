#!/usr/bin/env python3
"""w623-env-census-controls-v9c2.py — positive controls for the LENS_* configuration census.

⚠ BOTH ASSERTIONS ARE "THE RESIDUE IS EMPTY", which is the shape most likely to be vacuous. P5
is the one to read: it removes `.env.example` from the population, which is the exact mistake
this census made on its own first two runs and which made the answer wrong by SEVEN TIMES.

Rules, as in the earlier campaigns here: anchor count asserted before editing; a companion test
that must stay green; sha256 re-checked after revert.

Run from the repo root:  python3 scripts/w623-env-census-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

GUARD = "internal/config/env_census_test.go"
CONFIG = "internal/config/config.go"
ENVEX = ".env.example"
ATTEST = "cmd/lens/attestation_wiring.go"
PKG = "./internal/config/"

DOCUMENTED = "TestEveryEnvVarTheBinaryReadsIsDocumented"
CLAIMED = "TestEveryEnvVarConfigGoClaimsIsActuallyRead"
TEETH = "TestEnvCensusCanActuallyClassify"


def sha(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(p):
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


def write(p, s):
    with open(p, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(name):
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1"],
                       capture_output=True, text=True)
    return r.returncode == 0


CONTROLS = [
    ("P1", "a NEW env var is read and documented nowhere",
     [(ATTEST, '\tpem := os.Getenv("LENS_NVIDIA_ROOT_CA_PEM")',
       '\t_ = os.Getenv("LENS_ZZZ_UNDOCUMENTED_SETTING")\n\tpem := os.Getenv("LENS_NVIDIA_ROOT_CA_PEM")')],
     DOCUMENTED, CLAIMED, "the W4.22 class: a live gate no operator can find"),

    ("P2", "an existing setting loses its documentation line",
     [(ENVEX, "# LENS_NVIDIA_ROOT_CA_PEM=", "# LENS_NVIDIA_ROOT_CA_PEM_RENAMED=")],
     DOCUMENTED, CLAIMED,
     "the attestation trust anchor going undocumented again is precisely what W6.23 fixed"),

    # ⚠ THE GHOST DIRECTION — the one that actually found something.
    ("P3", "a config.go field comment names an env var nothing reads",
     [(CONFIG, "\tKeelInterval time.Duration // CONSTANT 1h (sweep tick) — no env var",
       "\tKeelInterval time.Duration // LENS_KEEL_INTERVAL (sweep tick, default 1h)")],
     CLAIMED, DOCUMENTED,
     "this is the exact comment W6.23 corrected: it read like the env-loaded fields around it "
     "and nothing read the name"),

    ("P4", "the read sweep counts a MENTION rather than an env call",
     [(GUARD, 'envRead  = regexp.MustCompile(`(?:Getenv|LookupEnv|parse[A-Za-z0-9_]*Env[A-Za-z0-9_]*)\\(\\s*"(LENS_[A-Z0-9_]+)"`)',
       'envRead  = regexp.MustCompile(`\\b(LENS_[A-Z0-9_]+)`)')],
     DOCUMENTED, TEETH,
     "LENS_EVENTS is a NATS stream name and LENS_NODE_TLS_CA is a comment about future support; "
     "counting mentions puts both in the residue"),

    # ⚠ THE POPULATION-BOUNDARY CONTROL, AND THE REASON THIS FILE EXISTS.
    ("P5", "`.env.example` is dropped from the documentation population",
     [(GUARD, '\t".env.example", "lens.env.example", "README.md",',
       '\t"lens.env.example", "README.md",')],
     DOCUMENTED, CLAIMED,
     "the census's own first two runs did exactly this and reported 57 and then 49 undocumented "
     "settings; the true answer was 3"),

    ("P6", "break the config.go field-comment parse",
     [(GUARD, 'fieldDoc = regexp.MustCompile(`^\\s*([A-Z][A-Za-z0-9_]*)\\s+\\S+\\s+//.*?(LENS_[A-Z0-9_]+)`)',
       'fieldDoc = regexp.MustCompile(`^ZZZ([A-Z]*)ZZZ(LENS_[A-Z0-9_]+)`)')],
     CLAIMED, TEETH, "the non-vacuity floor: a parse that finds no claims finds no ghosts"),

    ("P7", "the documentation check accepts anything",
     [(GUARD, '\tif _, ok := doc[n]; ok {\n\t\treturn true\n\t}', '\tif true {\n\t\treturn true\n\t}')],
     TEETH, CLAIMED,
     "\"every read name is documented\" is vacuous the moment the check cannot say no"),
]


def main():
    caught, missed = [], []
    for cid, desc, edits, must_red, must_green, note in CONTROLS:
        print(f"── {cid}: {desc}")
        if note:
            print(f"     ({note})")
        originals, shas = {}, {}
        ok = True
        for path, old, new in edits:
            if path not in originals:
                originals[path] = read(path)
                shas[path] = sha(path)
            cur = read(path)
            n = cur.count(old)
            if n != 1:
                print(f"   ✗ ANCHOR ERROR: {path} contains the anchor {n} times, want exactly 1")
                print(f"     anchor: {old[:110]!r}")
                ok = False
                break
            write(path, cur.replace(old, new, 1))
        if not ok:
            for path, s in originals.items():
                write(path, s)
            missed.append((cid, "anchor did not apply"))
            continue

        red_pass = run_test(must_red)
        green_pass = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect planted")
            missed.append((cid, "guard blind"))
        else:
            print(f"   ✓ CAUGHT by {must_red} (companion {must_green} stayed green)")
            caught.append(cid)

    print()
    print(f"CAUGHT {len(caught)}: {', '.join(caught) or '—'}")
    print(f"MISSED {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
