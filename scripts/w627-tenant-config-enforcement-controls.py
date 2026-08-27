#!/usr/bin/env python3
"""W6.27 control campaign — the tenant-config enforcement census and the four claim guards.

Each control: mutate ONE thing, assert a NAMED test goes RED and a NAMED companion stays GREEN,
restore, prove the restore by sha256.
"""
import hashlib, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAIN = os.path.join(ROOT, "cmd/lens/main.go")
CFG = os.path.join(ROOT, "internal/config/config.go")
STORE = os.path.join(ROOT, "internal/tenant/store.go")
TEST = os.path.join(ROOT, "internal/tenant/enforcement_census_test.go")
PROXY = os.path.join(ROOT, "internal/proxy/proxy.go")
FILES = [MAIN, CFG, STORE, TEST, PROXY]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/tenant/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new):
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want 1: %r" % (n, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))

CLAIM = "TestNoCommentClaimsTenantConfigIsEnforced"
READER = "TestTenantWorkspaceConfigHasNoEnforcementReader"
ALLOWED = "TestCheckAllowedHasNoProductionCaller"

CONTROLS = [
    ("U1 main.go's rate-limit claim restored", MAIN,
     "\t// Token-bucket multi-tier limiter (Item 8). ⚠ ONE TIER, NO CALLER.",
     "\t// Global tier is\n\t// configured from env; per-workspace tiers are layered on at\n"
     "\t// request time by callers that build a per-request limiter\n\t// from tenant.WorkspaceConfig.\n"
     "\t// Token-bucket multi-tier limiter (Item 8). ⚠ ONE TIER, NO CALLER.",
     CLAIM, ALLOWED, "the false present-tense rate-limit claim cannot come back"),

    ("U2 config.go's per-workspace-tier claim restored", CFG,
     "\t// Global rate limits (Item 8). Zero = no global cap.",
     "\t// Global rate limits (Item 8). Zero = no global cap; the\n"
     "\t// per-workspace tier in MultiTierLimiter still applies.",
     CLAIM, READER, "\"GlobalRPM=0 falls back to a per-workspace cap\" cannot come back"),

    ("U3 store.go's CheckAllowed claim restored", STORE,
     "// cache) is collected here.\n//\n// ⚠ WHAT IN HERE ACTUALLY GATES TRAFFIC",
     "// cache) is collected here. The model allowlist check `CheckAllowed`\n"
     "// returns an error the proxy can surface to the client without\n"
     "// round-tripping to Postgres.\n// ⚠ WHAT IN HERE ACTUALLY GATES TRAFFIC",
     CLAIM, ALLOWED, "the allowlist-is-enforced claim cannot come back"),

    ("U4 DefaultRetentionDays called a policy again", STORE,
     "\t// DefaultRetentionDays is the value stored when a workspace config leaves",
     "\t// DefaultRetentionDays is the policy when a workspace config\n"
     "\t// DefaultRetentionDays is the value stored when a workspace config leaves",
     CLAIM, READER, "a retention default cannot be described as an applied policy"),

    # U5 takes the shape a real wiring would: a function VALUE, not a call. The mutated file need
    # not compile — the guard reads the tree from disk, and `go test ./internal/tenant/` does not
    # build the proxy. The FIRST draft of this control used a call paren and the guard MISSED it,
    # which is how the missing-paren hole in the regex was found.
    ("U5 CheckAllowed wired as a function value", PROXY,
     "package proxy\n", "package proxy\n\nvar _ = tenant.CheckAllowed\n",
     ALLOWED, CLAIM, "wiring the allowlist by reference, not just by call, is caught"),

    ("U6 the PUT decode — the census's only expected reader — removed", MAIN,
     "\t\t\tvar in tenant.WorkspaceConfig", "\t\t\tvar in struct{ ID string }",
     READER, CLAIM, "the census counts a real occurrence; it is not green by construction"),

    ("U7 the file walk neutered", TEST,
     "\t\tif !strings.HasSuffix(path, \".go\") || strings.HasSuffix(path, \"_test.go\") {",
     "\t\tif true || !strings.HasSuffix(path, \".go\") || strings.HasSuffix(path, \"_test.go\") {",
     READER, CLAIM, "a broken walk is caught by the non-vacuity floor, not reported as clean"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-32s %s" % (os.path.basename(p), before[p]))
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
        hit = [l for l in red_out.splitlines() if "_test.go:" in l]
        if hit:
            print("     red says: %s" % hit[0].strip()[:140])
    results.append(verdict)
    print()

after = {p: sha(p) for p in FILES}
print("RESTORE PROOF")
clean = all(before[p] == after[p] for p in FILES)
for p in FILES:
    print("  %-32s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("Test")
print("\npackage green after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
