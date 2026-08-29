#!/usr/bin/env python3
"""W6.31 control campaign — the background-job classification census."""
import hashlib, os, signal, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MAIN = os.path.join(ROOT, "cmd/lens/main.go")
TEST = os.path.join(ROOT, "cmd/lens/background_job_classification_test.go")
# ⚠ Y4 AND Y5 MUTATE THE SCANNERS, WHICH MOVED OUT OF background_job_classification_test.go IN
# #526/#528. Both arms used to neuter a `regexp.MustCompile` declared in TEST; the census stopped
# being a line-and-substring rule and became a go/parser walk, the two regex vars were deleted, and
# the arms sat ANCHOR-FAILED ("appears 0 times") reporting 4/6 for an unknown period. A MOVED anchor
# is the worse half of that failure: the control was not near-missing, it was measuring nothing.
# ⚠ AND BOTH FILES BELONG IN FILES, NOT ONLY IN THE CONTROL ROWS: the sha256 restore proof at the
# bottom iterates FILES. An arm that mutates a file the proof does not cover is unproven to restore.
SCAN = os.path.join(ROOT, "cmd/lens/go_statement_scan_test.go")
LEADER = os.path.join(ROOT, "cmd/lens/leader_job_scan_test.go")
FILES = [MAIN, TEST, SCAN, LEADER]


def restore_on_signal(snapshot):
    """Put every snapshotted file back, then die of the signal we were sent.

    A `finally` DOES NOT RUN ON SIGTERM. Measured in talyvor-suite (W1.7, 78c69c8): a 2-minute
    command timeout killed a control mid-mutation and left a GATE REMOVED in the working tree,
    with a green suite and a `git status` showing only files the session had edited on purpose.
    Reproduced on demand in suite 5de27e3, talyvor-docs ffe9063 and talyvor-track 5f01947 — in the
    last two the file left mutated was go.mod.

    Re-raising with SIG_DFL keeps the exit status honest: a caller that killed this process still
    sees it die of that signal rather than exit 0 with a tidy tree. SIGKILL still strands and
    nothing in Python can change that.

    Deliberately self-contained rather than an import, so the next script is a paste. The
    population and the rule live in scripts/check-restore-signal-handlers.py.
    """
    def handler(signum, _frame):
        for path, blob in snapshot.items():
            try:
                open(path, "wb").write(blob)
            except OSError:
                pass
        sys.stderr.write("\n!! signal %d — restored %d mutated file(s) before exiting\n"
                         % (signum, len(snapshot)))
        signal.signal(signum, signal.SIG_DFL)
        os.kill(os.getpid(), signum)

    for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(s, handler)


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

    # ⚠ RE-POINTED FROM THE DELETED `anyGoroutine` REGEX TO THE WALK THAT REPLACED IT. The
    # property is unchanged and it is the one scanGoStatements' own docstring names: "a scan that
    # enumerates no goroutines reports every goroutine as classified". Dropping the collection
    # leaves the walk running and returning nothing, which is what a refactor that goes wrong here
    # actually looks like. It must red on the FLOOR (`found 0 leader-gated jobs, expected 30+`) and
    # WRM must stay green, because scanLeaderJobs is a different function.
    ("Y4 the goroutine scan neutered", SCAN,
     "\t\tout = append(out, site)",
     "\t\t_ = site",
     CLS, WRM, "a broken scan hits the floor rather than reporting every job classified"),

    # ⚠ RE-POINTED FROM THE DELETED `leaderGated` REGEX TO THE CONST THAT REPLACED IT. The
    # selector is matched against the RENDERED AST EXPRESSION of the call rather than against a
    # line, but it is still a name, and a name that stops matching is still how this fails. It
    # governs BOTH scanners, so no green-must-stay-green test is asserted — same as the original.
    ("Y5 the leader-gate selector stops matching", LEADER,
     'const leaderRunSelector = "haComps.leader.Run"',
     'const leaderRunSelector = "haComps.NOPE.Run"',
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
    # Installed AFTER the snapshot exists and re-installed each control, because `path`
    # differs per control. The `finally` below is the normal path; this is the one a
    # SIGTERM takes.
    restore_on_signal({path: backup.encode('utf-8')})
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
