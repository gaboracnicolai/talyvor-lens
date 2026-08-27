#!/usr/bin/env python3
"""W6.28 control campaign — the Tare conformance suite, every property mutated.

Each control: mutate ONE thing, assert a NAMED test goes RED and a NAMED companion stays GREEN,
restore, prove the restore by sha256.
"""
import hashlib, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONF = os.path.join(ROOT, "internal/tare/conformance_test.go")
FILES = [CONF]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/tare/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new):
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want 1: %r" % (n, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))

REG = "TestEveryReductionInThePackageIsRegistered"
ARM = "TestConformance_SamplesActuallyReduce"
DET = "TestConformance_IsDeterministic"
REF = "TestConformance_RefusalReturnsTheInputUnchangedWithNilError"
RT  = "TestConformance_LosslessReducersRoundTrip"
ANN = "TestConformance_LossyReducersAnnounceEveryElision"
KIN = "TestDeclaredKindsWithoutAReducer"

CONTROLS = [
    ("V1 a reducer dropped from the registry", CONF,
     '\t\t\tname: "JSONReducer", kind: tare.KindJSON,',
     '\t\t\tname: "JSONReducer_UNREGISTERED", kind: tare.KindJSON,',
     REG, ANN, "a reducer held to no properties at all is caught by the source scan"),

    ("V2 the lossy reducer declares itself lossless", CONF,
     "\t\t\tlossy:     true,\n\t\t\tmarker:    tare.ElisionMarker,",
     "\t\t\tlossy:     false,\n\t\t\tmarker:    tare.ElisionMarker,",
     RT, REG, "a losslessness claim with no inverse to demonstrate it is refused"),

    ("V3 the lossy reducer's marker removed", CONF,
     "\t\t\tmarker:    tare.ElisionMarker,", '\t\t\tmarker:    "",',
     ANN, RT, "dropped content that does not announce itself is caught"),

    ("V4 the source scan neutered", CONF,
     '\t\tif !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {',
     '\t\tif true || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {',
     REG, ANN, "a broken scan hits the floor rather than reporting a clean registry"),

    ("V5 the lossless inverse replaced by a pass-through", CONF,
     "\t\t\tinverse: tare.ExpandJSON,",
     "\t\t\tinverse: func(b []byte) ([]byte, error) { return b, nil },",
     RT, ANN, "an inverse that does not actually invert cannot certify losslessness"),

    ("V6 a reducer registered for the unbuilt log kind", CONF,
     "\t\t\tname: \"GoBodyTrimmer\", kind: tare.KindCode,",
     "\t\t\tname: \"GoBodyTrimmer\", kind: tare.KindLog,",
     KIN, RT, "the must-keep tripwire fires the day a log reducer appears"),

    ("V7 a sample that does not actually reduce", CONF,
     '\t\t[]byte(`{"items":[{"id":1,"name":"a","ok":true}',
     '\t\t[]byte(`{"a":1}`), []byte(`{"items":[{"id":1,"name":"a","ok":true}',
     ARM, REG, "an inert sample makes every property vacuous and is caught first"),

    ("V9 the PrefixStable exclusion's justification removed", CONF,
     '\t\t"TestPrefix_OnlyTheNewestMessageChanges",',
     '\t\t"TestPrefix_OnlyTheNewestMessageChangeX",',
     REG, RT, "an exclusion that names a file is checked against the file (the W6.26 lesson)"),

    ("V8 the refusal contract broken by feeding it reducible content", CONF,
     '\t\t\twrongKind: []byte(`{"a":1}`),',
     '\t\t\twrongKind: []byte(`[{"index":0,"status":"ok","message":"FIRST"},{"index":1,"status":"ok","message":"x"},{"index":2,"status":"e","message":"y"}]`),',
     REF, RT, "the refusal test drives a real refusal, not content the reducer happens to reduce"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-28s %s" % (os.path.basename(p), before[p]))
ok, out = run("Test")
if not ok:
    sys.exit("package not green before the campaign:\n" + out[-2500:])
print("\nbaseline: package GREEN\n")

results = []
for name, path, old, new, red, green, proves in CONTROLS:
    backup = open(path).read()
    try:
        anchored(path, old, new)
        red_ok, red_out = run(red)
        green_ok, _ = run(green)
        verdict = "CAUGHT" if (not red_ok and green_ok) else ("MISSED" if red_ok else "COLLATERAL")
    except AssertionError as e:
        verdict, red_out = "ANCHOR-FAILED: %s" % e, ""
    finally:
        open(path, "w").write(backup)
    print("%-52s %s" % (name, verdict))
    print("     proves: %s" % proves)
    if verdict == "CAUGHT":
        hit = [l for l in red_out.splitlines() if "conformance_test.go:" in l]
        if hit:
            print("     red says: %s" % hit[0].strip()[:145])
    results.append(verdict)
    print()

after = {p: sha(p) for p in FILES}
clean = all(before[p] == after[p] for p in FILES)
print("RESTORE PROOF")
for p in FILES:
    print("  %-28s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("Test")
print("\npackage green after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
