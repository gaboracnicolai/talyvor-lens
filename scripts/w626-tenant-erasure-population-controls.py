#!/usr/bin/env python3
"""W6.26 control campaign — every clause of the widened tenant-erasure population, mutated.

Each control: mutate ONE thing, assert a NAMED test goes RED, assert a NAMED companion stays
GREEN (so the mutation is not simply breaking the package), restore, prove the restore by sha256.

A control with no green companion proves only that something broke. A control with no restore
proof leaves the tree quietly mutated. Both have happened in this queue.
"""
import hashlib, os, re, subprocess, sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MANIFEST = os.path.join(ROOT, "internal/tenantdata/manifest.go")
TEST = os.path.join(ROOT, "internal/tenantdata/manifest_test.go")
DBURL = os.environ.get("LENS_TEST_DATABASE_URL")
if not DBURL:
    sys.exit("LENS_TEST_DATABASE_URL is required — these guards read the live migrated schema")

def sha(p): return hashlib.sha256(open(p, "rb").read()).hexdigest()

def run(test):
    r = subprocess.run(["go", "test", "-count=1", "-run", "^%s$" % test, "./internal/tenantdata/"],
                       cwd=ROOT, capture_output=True, text=True, env={**os.environ})
    return r.returncode == 0, (r.stdout + r.stderr)

ANCHORS = {}
def anchored(path, old, new):
    """Substitution that refuses to be a no-op — a control whose edit did not apply is a control
    that proves the guard catches nothing."""
    s = open(path).read()
    n = s.count(old)
    if n != 1:
        raise AssertionError("anchor appears %d times, want exactly 1: %r" % (n, old[:70]))
    open(path, "w").write(s.replace(old, new, 1))

CONTROLS = [
    # (name, file, old, new, must_go_red, must_stay_green, what_it_proves)
    ("T1 population reverted to a bare workspace_id scan", TEST,
     "\t  AND col.column_name ILIKE '%workspace%'`", "\t  AND col.column_name = 'workspace_id'`",
     "TestPopulationIsStrictlyWiderThanABareWorkspaceIDScan",
     "TestCascadeOnlyChildrenStillReachADeleteParent",
     "the widening is guarded — reverting to the pre-W6.26 population is caught"),

    ("T2 relkind filter removed", TEST,
     "\t  AND c.relkind IN ('r','p')\n\t  AND NOT c.relispartition\n\t  AND col.column_name ILIKE '%workspace%'",
     "\t  AND NOT c.relispartition\n\t  AND col.column_name ILIKE '%workspace%'",
     "TestPopulationExcludesViews", "TestCascadeReachabilityDiscriminates",
     "views re-enter the population and are caught — the false-finding direction"),

    ("T3 workspace_configs unclassified", MANIFEST,
     '\t"workspace_configs": {Delete, "W6.26; keyed by `id` like `workspaces`.',
     '\t"__removed_workspace_configs": {Delete, "W6.26; keyed by `id` like `workspaces`.',
     "TestManifestCoversEveryTenantScopedTable", "TestPopulationExcludesViews",
     "the actual defect W6.26 found is caught if it is reintroduced"),

    ("T4 reachedByCascade always true", TEST,
     "\tfor _, p := range parents[table] {", "\tif true {\n\t\treturn true\n\t}\n\tfor _, p := range parents[table] {",
     "TestCascadeReachabilityDiscriminates", "TestMappingTablesExistAndAreClassified",
     "a permissive reachability helper would hide every real gap"),

    ("T5 reachedByCascade always false", TEST,
     "\tif depth > 16 {\n\t\treturn false", "\tif true {\n\t\treturn false\n\t}\n\tif depth > 16 {\n\t\treturn false",
     "TestCascadeOnlyChildrenStillReachADeleteParent", "TestPopulationExcludesViews",
     "an inert reachability helper would manufacture five false gaps"),

    ("T6 sessions reclassified Retain (cascade into an undeletable root)", MANIFEST,
     '"sessions":                      {Delete, ""},', '"sessions":                      {Retain, "control T6"},',
     "TestCascadeOnlyChildrenStillReachADeleteParent", "TestPopulationExcludesViews",
     "the cascade root must be Delete, not merely present — 0055 makes Retain rows undeletable"),

    ("T7 workspace_configs dropped from MappingTables", MANIFEST,
     'var MappingTables = []string{"workspaces", "workspace_configs"}',
     'var MappingTables = []string{"workspaces"}',
     "TestMappingTablesExistAndAreClassified", "TestCascadeOnlyChildrenStillReachADeleteParent",
     "the one hand-declared part of the population cannot silently shrink back"),

    ("T8 a classification loses its reason", MANIFEST,
     '"routing_prediction_mints": {Delete, "W6.26; key `contributor_workspace_id`. Not audit-guarded."},',
     '"routing_prediction_mints": {Delete, ""},',
     "TestW626EntriesCarryTheirReason", "TestPopulationExcludesViews",
     "a bare classification on a never-reviewed table is caught"),
]

before = {MANIFEST: sha(MANIFEST), TEST: sha(TEST)}
print("BASELINE sha256\n  manifest.go      %s\n  manifest_test.go %s\n" % (before[MANIFEST], before[TEST]))

# The whole package must be green before any of this means anything.
ok, out = run("Test")
if not ok:
    sys.exit("package is not green before the campaign:\n" + out[-3000:])
print("baseline: package GREEN\n")

results = []
for name, path, old, new, red, green, proves in CONTROLS:
    backup = open(path).read()
    try:
        anchored(path, old, new)
        red_ok, red_out = run(red)
        green_ok, green_out = run(green)
        verdict = "CAUGHT" if (not red_ok and green_ok) else \
                  ("MISSED" if red_ok else "COLLATERAL(companion also red)")
    except AssertionError as e:
        verdict, red_out = "ANCHOR-FAILED: %s" % e, ""
    finally:
        open(path, "w").write(backup)
    print("%-52s %s" % (name, verdict))
    print("     proves: %s" % proves)
    if verdict == "CAUGHT":
        first = [l for l in red_out.splitlines() if "manifest" in l and (".go:" in l)]
        if first:
            print("     red says: %s" % first[0].strip()[:150])
    results.append((name, verdict))
    print()

after = {MANIFEST: sha(MANIFEST), TEST: sha(TEST)}
print("RESTORE PROOF")
clean = True
for p in (MANIFEST, TEST):
    same = before[p] == after[p]
    clean &= same
    print("  %-18s %s  %s" % (os.path.basename(p), "IDENTICAL" if same else "!! MUTATED !!", after[p]))

ok, _ = run("Test")
print("\npackage green after restore: %s" % ok)
caught = sum(1 for _, v in results if v == "CAUGHT")
print("\n%d/%d controls CAUGHT" % (caught, len(results)))
sys.exit(0 if (caught == len(results) and clean and ok) else 1)
