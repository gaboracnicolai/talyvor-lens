#!/usr/bin/env python3
"""W6.31 control campaign — the background-job classification census."""
import hashlib, os, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAIN = os.path.join(ROOT, "cmd/lens/main.go")
TEST = os.path.join(ROOT, "cmd/lens/background_job_classification_test.go")
FILES = [MAIN, TEST]

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./cmd/lens/"],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr

def anchored(path, old, new):
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want 1: %r" % (n, old[:60]))
    open(path, "w").write(s.replace(old, new, 1))

CLS = "TestEveryBackgroundGoroutineIsClassified"
WRM = "TestTheCacheWarmerIsLeaderGated"

CONTROLS = [
    ("Y1 the warmer un-gated again", MAIN,
     'go haComps.leader.Run(ctx, "cache-warmer", 30*time.Second, func(lctx context.Context) {\n\t\tcacheWarmer.Start(lctx, 1*time.Hour)\n\t})',
     'go cacheWarmer.Start(ctx, 1*time.Hour)',
     WRM, None, "the money-spending job cannot drift back to running on every replica"),

    ("Y2 a new ungated background job appears", MAIN,
     "\tgo cpSyncer.Run(ctx, 30*time.Second)",
     "\tgo cpSyncer.Run(ctx, 30*time.Second)\n\tgo semanticCache.DeleteStale(ctx)",
     CLS, WRM, "an unclassified goroutine is caught rather than inherited"),

    ("Y3 a per-replica classification loses its reason", TEST,
     '"cpSyncer.Run": "IN-PROCESS STATE. Rebuilds this replica\'s compression-policy cache; main.go " +\n\t\t"says so directly at the reload-interval comment — each replica must refresh its OWN cache.",',
     '"cpSyncer.Run": "",',
     CLS, WRM, "a bare classification is a label, not a decision"),

    ("Y4 the goroutine scan neutered", TEST,
     'var anyGoroutine = regexp.MustCompile(`^\\s*go `)',
     'var anyGoroutine = regexp.MustCompile(`^\\s*NEVERMATCH `)',
     CLS, WRM, "a broken scan hits the floor rather than reporting every job classified"),

    ("Y5 the leader-gate pattern stops matching", TEST,
     'var leaderGated = regexp.MustCompile(`^\\s*go haComps\\.leader\\.Run\\(ctx, "([a-z0-9-]+)"`)',
     'var leaderGated = regexp.MustCompile(`^\\s*go haComps\\.NOPE\\.Run\\(ctx, "([a-z0-9-]+)"`)',
     CLS, None, "the census cannot pass by seeing zero singletons"),

    ("Y6 both a gated AND an ungated warmer", MAIN,
     "\tcacheWarmer := warmer.New(pool, l, exactCache, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey)",
     "\tcacheWarmer := warmer.New(pool, l, exactCache, cfg.OpenAIAPIKey, cfg.AnthropicAPIKey)\n\tgo cacheWarmer.Start(ctx, 1*time.Hour)",
     WRM, None, "a leftover ungated call beside the gated one is caught"),
]

before = {p: sha(p) for p in FILES}
print("BASELINE sha256")
for p in FILES:
    print("  %-42s %s" % (os.path.basename(p), before[p]))
ok, out = run("TestEveryBackgroundGoroutineIsClassified|TestTheCacheWarmerIsLeaderGated")
if not ok:
    sys.exit("not green before the campaign:\n" + out[-2500:])
print("\nbaseline: GREEN\n")

results = []
for name, path, old, new, red, green, proves in CONTROLS:
    backup = open(path).read()
    try:
        anchored(path, old, new)
        red_ok, red_out = run(red)
        green_ok = True
        if green:
            green_ok, _ = run(green)
        verdict = "CAUGHT" if (not red_ok and green_ok) else ("MISSED" if red_ok else "COLLATERAL")
    except AssertionError as e:
        verdict, red_out = "ANCHOR-FAILED: %s" % e, ""
    finally:
        open(path, "w").write(backup)
    print("%-46s %s" % (name, verdict))
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
    print("  %-42s %s" % (os.path.basename(p), "IDENTICAL" if before[p] == after[p] else "!! MUTATED !!"))
ok, _ = run("TestEveryBackgroundGoroutineIsClassified|TestTheCacheWarmerIsLeaderGated")
print("\ngreen after restore: %s" % ok)
c = results.count("CAUGHT")
print("\n%d/%d controls CAUGHT" % (c, len(results)))
sys.exit(0 if (c == len(results) and clean and ok) else 1)
