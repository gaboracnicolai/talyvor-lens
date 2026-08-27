#!/usr/bin/env python3
"""w622-orphan-relation-controls-v9c2.py — positive controls for the orphan-relation census.

⚠ THIS ITEM REPAIRS NOTHING. Dropping a table is a migration and a decision; correcting a
comment inside migrations/0016_experiments.sql would mean editing applied migration history,
which is not a thing to do casually. So everything merged is a RECORD, and a record that
cannot fail is worth nothing.

O1 is the one to read: it makes a recorded orphan USED, which must fire — a census that stays
quiet while its subject changes is worse than no census on a claim somebody will act on.

Rules, as in the earlier campaigns here. Needs LENS_TEST_DATABASE_URL.

Run from the repo root:  python3 scripts/w622-orphan-relation-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

GUARD = "internal/schemasql/orphan_relation_test.go"
MANIFEST = "internal/tenantdata/manifest.go"
ENGINE = "internal/guardrails/engine.go"
PKG = "./internal/schemasql/"

CENSUS = "TestOrphanRelationCensus"
TEETH = "TestOrphanCensusCanActuallyClassify"
NOSTAR = "TestNoSelectStarMakesTheCensusAnswerable"
PARITY = "TestEveryWrittenColumnExistsInTheMigratedSchema"


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
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1", "-v"],
                       capture_output=True, text=True)
    return r.returncode == 0, ("--- SKIP: " + name) in r.stdout


CONTROLS = [
    # ⚠ THE DIRECTION THAT MATTERS. Somebody acts on this record — puts ab_tests back to work,
    # or drops it — and the record must not stay quiet.
    ("O1", "a recorded orphan comes into USE — the record must fire, not go quiet",
     [(ENGINE, "const upsertPolicySQL = `INSERT INTO guardrail_policies (workspace_id, policy)",
       "const probeSQL = `INSERT INTO ab_tests (id) VALUES ($1)`\n\n"
       "const upsertPolicySQL = `INSERT INTO guardrail_policies (workspace_id, policy)")],
     CENSUS, TEETH, "a stale record on a 'this is dead, drop it' claim is how a live table gets dropped"),

    ("O2", "a NEW orphan appears — a relation nothing names and nobody recorded",
     [(GUARD, '\t"branch_spend": "ORPHAN, and accurately documented as one:',
       '\t"zzz_unrecorded_orphan": "deliberately absent from the schema so the census must "+\n'
       '\t\t"complain about the RECORD rather than the tree",\n'
       '\t"branch_spend": "ORPHAN, and accurately documented as one:')],
     CENSUS, TEETH, "the mirror: a recorded orphan the schema does not have"),

    ("O3", "a guardrail_events WRITER appears — it stops being a deletion promise over nothing",
     [(ENGINE, "const upsertPolicySQL = `INSERT INTO guardrail_policies (workspace_id, policy)",
       "const eventsSQL = `INSERT INTO guardrail_events (workspace_id) VALUES ($1)`\n\n"
       "const upsertPolicySQL = `INSERT INTO guardrail_policies (workspace_id, policy)")],
     CENSUS, TEETH,
     "the finding is that the tenant-delete manifest promises to erase rows nothing writes"),

    ("O4", "the Go-string exclusion is removed — the parameterised writers look like orphans",
     [(GUARD, "\t\tif inGoString[low] {\n\t\t\tonlyGoString = append(onlyGoString, r)\n\t\t\tcontinue\n\t\t}",
       "\t\tif false && inGoString[low] {\n\t\t\tonlyGoString = append(onlyGoString, r)\n\t\t\tcontinue\n\t\t}")],
     CENSUS, TEETH,
     "poolroyalty's tables are named by a Go constant; calling them orphans is the flattering error"),

    ("O5", "the partition exclusion is removed — 24 partitions flood the orphan set",
     [(GUARD, "AND c.relispartition = false", "AND c.relispartition IN (false, true)")],
     CENSUS, TEETH, "excluded mechanically via pg_class, not by a name rule, and this proves it matters"),

    ("O6", "break the SQL sweep — every relation looks unnamed",
     [(GUARD, '\tsqlish := regexp.MustCompile(`(?i)\\b(SELECT|INSERT|UPDATE|DELETE|RETURNING|ON CONFLICT|TRUNCATE)\\b`)',
       '\tsqlish := regexp.MustCompile(`(?i)\\bZZZNOSUCHVERBZZZ\\b`)')],
     CENSUS, PARITY, "the non-vacuity floor: a blind sweep calls everything an orphan"),

    # ⚠ THE PREMISE. Without it the whole census is unfalsifiable.
    ("O7", "a `SELECT *` appears — the census stops being answerable",
     [(MANIFEST, "package tenantdata",
       "package tenantdata\n\nconst probeStarSQL = `SELECT * FROM workspaces`")],
     NOSTAR, TEETH,
     "a star reads columns without naming them; the guard must say the question became unanswerable"),
]


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("⚠ LENS_TEST_DATABASE_URL unset — controls needing the schema will report UNARMED.\n")
    caught, missed, unarmed = [], [], []
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

        red_pass, red_skip = run_test(must_red)
        green_pass, green_skip = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if red_skip:
            print(f"   ⚠ UNARMED: {must_red} SKIPPED (needs a database)")
            unarmed.append((cid, must_red))
            continue
        if not green_skip and not green_pass:
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
    print(f"CAUGHT  {len(caught)}: {', '.join(caught) or '—'}")
    print(f"UNARMED {len(unarmed)}: {', '.join(c for c, _ in unarmed) or '—'}")
    print(f"MISSED  {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
