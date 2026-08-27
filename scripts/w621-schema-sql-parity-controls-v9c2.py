#!/usr/bin/env python3
"""w621-schema-sql-parity-controls-v9c2.py — positive controls for the schema/SQL parity guard.

⚠ THE GUARD PASSED CLEAN ON ITS FIRST RUN — 1036 column references and 180 target tables, zero
mismatches. That is the result, and it is worth nothing until something shows the check can
fail. These controls plant real defects in real files, in each of the four forms the guard reads.

N5 is the one to read: it plants a wrong TABLE name, which the column check cannot see at all
(an unknown table is skipped, not flagged) and which exists only because that hole was measured
and closed with its own assertion.

Rules, as in the earlier campaigns here: anchor count asserted before editing; a companion test
that must stay green; sha256 re-checked after revert. Needs LENS_TEST_DATABASE_URL — the guard
builds the real migrated schema, so without a database every control reports UNARMED.

Run from the repo root:  python3 scripts/w621-schema-sql-parity-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

GUARD = "internal/schemasql/schema_sql_parity_test.go"
PKG = "./internal/schemasql/"

COLS = "TestEveryWrittenColumnExistsInTheMigratedSchema"
TEETH = "TestParityCheckCanActuallyFail"
TABLES = "TestEveryWrittenTableExistsInTheMigratedSchema"


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def write(path, s):
    with open(path, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(name):
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1", "-v"],
                       capture_output=True, text=True)
    return r.returncode == 0, ("--- SKIP: " + name) in r.stdout, r.stdout + r.stderr


CONTROLS = [
    ("N1", "an INSERT column list names a column the schema does not have",
     [("internal/mining/compute_mining.go",
       "\t\tINSERT INTO inference_nodes\n\t\t\t(workspace_id, url, provider, models, gpu_type, max_concurrent, price_per_token, ed25519_pubkey)",
       "\t\tINSERT INTO inference_nodes\n\t\t\t(workspace_id, url, provider, models, gpu_type, max_concurrent, price_per_tokenn, ed25519_pubkey)")],
     COLS, TABLES, "one letter — the shape a rename leaves behind"),

    ("N2", "an UPDATE SET names a column the schema does not have",
     [("internal/mining/compute_mining.go",
       "`UPDATE inference_nodes SET verified = TRUE WHERE id = $1`",
       "`UPDATE inference_nodes SET verifiedd = TRUE WHERE id = $1`")],
     COLS, TABLES, "the write path W6.11 measured, with its column renamed out from under it"),

    ("N3", "an ON CONFLICT DO UPDATE SET names a column the schema does not have",
     [("internal/mining/traffic_holds.go",
       "    ON CONFLICT (request_id, workspace_id, mint_type) DO NOTHING`",
       "    ON CONFLICT (request_id, workspace_id, mint_type) DO UPDATE SET minted_amountt = 1`")],
     COLS, TABLES,
     "these 97 references were INVISIBLE to the first draft, which read the upsert's table as \"set\""),

    ("N4", "break the sweep — the SQL-literal regex matches nothing",
     [(GUARD, 'sqlLiteral = regexp.MustCompile("`([^`]*)`")',
       'sqlLiteral = regexp.MustCompile("ZZZ([^`]*)ZZZ")')],
     COLS, TEETH, "the non-vacuity floor: a sweep that reads no SQL reports clean parity"),

    # ⚠ THE HOLE THAT WAS MEASURED AND CLOSED. A wrong TABLE name is invisible to the column
    # check — an unknown table is skipped, not flagged.
    ("N5", "an INSERT names a TABLE the schema does not have",
     [("internal/mining/embedding_mining.go",
       "\t\tINSERT INTO embedding_nodes\n", "\t\tINSERT INTO embedding_nodess\n")],
     TABLES, COLS,
     "the column check CANNOT see this: it skips tables it does not know, so the table "
     "assertion is the only thing standing between a typo and a runtime failure"),

    # ⚠ N6 AND N7 NAME AN EXPECTED MESSAGE INSTEAD OF A GREEN COMPANION, AND THE FIRST DRAFT
    # GOT THAT WRONG. Both mutations empty the SCHEMA, and every test in this package reads the
    # schema — so the companion failed too and the harness (correctly) refused to score it as a
    # catch. But "the companion broke" is the wrong question here: the property under test is
    # that the guard's NON-VACUITY FLOOR fires rather than the guard quietly reporting clean
    # parity over nothing. So these two assert on the failure MESSAGE, which is stronger than a
    # companion: it proves the guard detected the empty schema deliberately.
    ("N6", "break the schema read — the guard compares against an empty schema",
     [(GUARD, "WHERE table_schema = 'public'", "WHERE table_schema = 'no_such_schema'")],
     COLS, "want >= 100", "an empty schema makes every table unknown and therefore skipped"),

    ("N7", "the migrations are not applied — the schema is empty for a different reason",
     [(GUARD, "applied, err := dbmigrate.Run(ctx, conn, migrations.FS)",
       "applied, err := []string{\"faked\"}, error(nil)\n\t_ = migrations.FS\n\t_ = dbmigrate.Run")],
     COLS, "want >= 100",
     "the guard must build the schema the way a deployment does, not assume somebody else did"),
]


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("⚠ LENS_TEST_DATABASE_URL is unset — this guard builds the real migrated schema, so")
        print("  every control below will report UNARMED. Re-run with a database.\n")

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

        red_pass, red_skip, red_out = run_test(must_red)
        expect_msg = must_green.startswith("want ") or must_green.startswith("no ")
        if expect_msg:
            green_pass, green_skip = True, False
        else:
            green_pass, green_skip, _ = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if red_skip:
            print(f"   ⚠ UNARMED: {must_red} SKIPPED (needs a database)")
            unarmed.append((cid, must_red))
            continue
        if expect_msg:
            if red_pass:
                print(f"   ✗ MISSED: {must_red} still PASSES with the defect planted")
                missed.append((cid, "guard blind"))
            elif must_green not in red_out:
                print(f"   ✗ WRONG FAILURE: {must_red} failed but not with the non-vacuity floor "
                      f"message {must_green!r} — it may have failed for an unrelated reason")
                missed.append((cid, "wrong failure"))
            else:
                print(f"   ✓ CAUGHT by {must_red}, and it failed on its NON-VACUITY FLOOR "
                      f"({must_green!r}) rather than by accident")
                caught.append(cid)
            continue
        if not green_skip and not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build "
                  f"or the schema itself")
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
