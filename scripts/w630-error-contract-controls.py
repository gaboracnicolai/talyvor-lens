#!/usr/bin/env python3
"""W6.30 control campaign — the error-contract guard.

⚠ THIS GUARD PINS A KNOWN MISMATCH, so it is green on a clean tree by design. That makes the
controls the ONLY evidence it is not vacuous: each mutation moves one side of the mismatch and the
guard must notice — including the mutations that REPAIR it, because a silent repair leaves W6.30
open in the queue against a tree where it no longer applies.
"""
import hashlib, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
AUTH = os.path.join(ROOT, "internal/auth/manager.go")
RL   = os.path.join(ROOT, "internal/ratelimit/middleware.go")
SPEC = os.path.join(ROOT, "internal/api/openapi.go")
TEST = os.path.join(ROOT, "internal/apicontract/error_contract_test.go")
FILES = [AUTH, RL, SPEC, TEST]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/apicontract/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new):
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want 1: %r" % (n, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))


# X4 must REPLACE the anthropic operation's existing responses map, not add a second one — the
# first draft appended a `"responses":` key beside the one already there, which is a duplicate-key
# COMPILE error rather than a behaviour change, and the campaign correctly reported COLLATERAL
# instead of pretending it had proved something.
X4_OLD = (
    '\t\t\t\t"summary": "Proxy to Anthropic",\n'
    '\t\t\t\t"parameters": []map[string]any{\n'
    '\t\t\t\t\t{"name": "path", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},\n'
    '\t\t\t\t},\n'
    '\t\t\t\t"responses": map[string]any{\n'
    '\t\t\t\t\t"200": map[string]any{"description": "proxied response"},\n'
    '\t\t\t\t},\n'
)
X4_NEW = X4_OLD.replace(
    '\t\t\t\t\t"200": map[string]any{"description": "proxied response"},\n',
    '\t\t\t\t\t"200": map[string]any{"description": "proxied response"},\n\t\t\t\t\t"401": errResp("401"),\n')

DOC  = "TestExactlyTheProxyOpenAIOperationDocumentsItsErrors"
WIRE = "TestTheDocumentedErrorResponsesDoNotMatchTheDocument"
CODE = "TestTheErrorCodeConstantsAreEmittedNowhere"

CONTROLS = [
    ("X1 the 401 half-repaired to the schema", AUTH,
     '_, _ = w.Write([]byte(`{"error":"unauthorized"}`))\n\t\t\t\treturn',
     '_, _ = w.Write([]byte(`{"error":"unauthorized","code":"UNAUTHORIZED"}`))\n\t\t\t\treturn',
     WIRE, DOC, "a partial move toward the schema is caught — code without message satisfies nobody"),

    ("X2 the 401 drops the key every client parses", AUTH,
     '{"error":"unauthorized"}`))\n\t\t\t\treturn',
     '{"detail":"unauthorized"}`))\n\t\t\t\treturn',
     WIRE, DOC, "a breaking change to the de-facto error contract is caught"),

    ("X3 APIError stops requiring anything", SPEC,
     '"required": []string{"code", "message"},', '"required": []string{},',
     WIRE, CODE, "an empty required list makes every body conform and would prove nothing"),

    ("X4 the anthropic twin starts documenting errors", SPEC, X4_OLD, X4_NEW,
     DOC, CODE, "a new promise about an error body must be checked against the wire"),

    ("X5 the 429 promise withdrawn", SPEC,
     '\t\t\t\t\t"429": errResp("429"),\n', "",
     DOC, CODE, "removing a published error response is noticed, not silently accepted"),

    ("X6 the rate limiter starts emitting a declared code", RL,
     '"error":               "rate limit exceeded",',
     '"error":               "rate limit exceeded",\n\t\t\t\t\t"code":                "RATE_LIMITED",',
     CODE, DOC, "the day the nine dead codes go live, both sides must move together"),

    ("X7 the 401 fixture stops rejecting", TEST,
     'if rec.Code != http.StatusUnauthorized {',
     'if rec.Code == http.StatusUnauthorized {',
     WIRE, DOC, "the fixture is armed — it drives a real rejection, not an empty recorder"),

    ("X8 the 429 fixture stops rejecting", TEST,
     'l := ratelimit.New(rdb, []ratelimit.RateRule{{RequestsPerSecond: 1}})',
     'l := ratelimit.New(nil, []ratelimit.RateRule{{RequestsPerSecond: 1}})',
     WIRE, DOC, "a limiter that never rejects is caught rather than passing over nothing"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-26s %s" % (os.path.basename(p), before[p]))
ok, out = run("Test")
if not ok:
    sys.exit("package not green before the campaign:\n" + out[-2500:])
print("\nbaseline: package GREEN (by design — it pins a mismatch)\n")

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
    print("%-50s %s" % (name, verdict))
    print("     proves: %s" % proves)
    if verdict == "CAUGHT":
        hit = [l for l in red_out.splitlines() if "_test.go:" in l]
        if hit:
            print("     red says: %s" % hit[0].strip()[:140])
    results.append(verdict)
    print()

after = {p: sha(p) for p in FILES}
clean = all(before[p] == after[p] for p in FILES)
print("RESTORE PROOF")
for p in FILES:
    print("  %-26s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("Test")
print("\npackage green after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
