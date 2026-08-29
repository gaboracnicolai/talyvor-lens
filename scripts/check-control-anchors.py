#!/usr/bin/env python3
"""check-control-anchors.py — the dry-run anchor census for this repo's control scripts.

⚠ WHAT THIS ANSWERS. A control script mutates a tracked file by string-replacing an ANCHOR, and
asserts the anchor occurs exactly once before it does. When the code moves or multiplies, the
anchor stops resolving and THE CONTROL STOPS MEASURING ANYTHING. It still exits non-zero and still
names the anchor — but nothing in CI runs these scripts, so the loudness reaches nobody, and the
only signal a human gets is a fraction like `4/6` that reads like a SCORE rather than like an
instrument reporting it could not run.

Measured here (W6.45): `w631-background-job-classification-controls.py` sat at `4/6` for an unknown
period with two arms `ANCHOR-FAILED: anchor appears 0 times`, because #526/#528 replaced a regex
census with a go/parser walk and deleted the two vars those arms mutated. `0 times` is the worse
half: the subject MOVED, so the control was not near-missing, it was measuring nothing. The sibling
repos hit the other direction — talyvor-docs `2 times` and talyvor-track `3 times`, where a literal
multiplied.

⚠⚠ WHAT THIS DOES NOT ANSWER, AND IT IS PRINTED ON EVERY RUN INCLUDING A GREEN ONE.
IT READS 6 OF THIS REPO'S 34 CONTROL SCRIPTS. A green here is NOT a statement that the estate's
anchors are healthy — it is a statement about the six it can read. That distinction is the whole
reason this file prints its own denominator instead of a tick.

The precedent for saying so out loud is in this repo already: `scripts/check-realpg-dsn.sh` records
that talyvor-docs shipped a guard selecting real-PG tests BY NAME, which covered 36 of 78 (46%)
while a passing step implied protection over all of them, and REMOVED it. A partial guard is worth
having; a partial guard that reads as total is not.

⚠⚠⚠ WHY ONLY SIX, MEASURED RATHER THAN ASSUMED. A row is readable when one of its elements
resolves to an existing FILE — then the element IMMEDIATELY AFTER it is the `old` anchor, by the
(name, PATH, old, new, ...) convention. 251 candidate rows exist; 203 have no resolvable file
because their table does not carry a path column, and the file is a module constant used elsewhere.
The six readable scripts are w626–w631 CONSECUTIVELY: the convention was adopted at a point in
time, so coverage grows only as new scripts adopt it. It is not a property of the estate.

⚠ AND THE POSITIONAL RULE IS DELIBERATE, NOT LAZY. A looser "first long string after the path"
heuristic would sometimes pick the row's `new` value — a literal that must NOT be present, and
which is indistinguishable from a stale `old` that is also not present. Every false positive and
every true finding would look identical (talyvor-docs W6.43 records exactly this). Taking strictly
position pi+1 means this never reads a `new` at all. MEASURED: 48/48 rows resolve, 0 mismatches.

Pure string counting over the tracked tree: no mutation, no `go test`, no database.
"""
import ast, argparse, os, pathlib, sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = ROOT / "scripts"

# ── FLOORS, ONE PER DIMENSION ────────────────────────────────────────────────────────────────
# ⚠ A SINGLE FLOOR OVER THE UNION IS SATISFIED BY EITHER HALF ALONE. Separate floors are the
# lesson talyvor-suite 4829830 paid for: stub one detector and the other holds the count up, so the
# thing the guard exists to check reverts to silence with the guard green.
#
# WALK_FLOOR is a VACUITY floor and is deliberately loose: it catches the glob returning nothing or
# almost nothing (wrong cwd, broken pattern), not a script being legitimately deleted.
# READABLE_FLOOR and ROW_FLOOR are the COVERAGE claim and are deliberately TIGHT: if a readable
# script drifts out of the readable shape, coverage shrinks in silence and everything still passes.
# That is the failure this file is most likely to have, so it is the one with no slack.
WALK_FLOOR = 20      # scripts/*.py that parse
READABLE_FLOOR = 6   # scripts contributing at least one censusable row
ROW_FLOOR = 48       # censusable (file, anchor) pairs


def const_env(tree):
    """Resolve module-level string constants, including `os.path.join(ROOT, "...")`."""
    env = {"ROOT": str(ROOT)}
    for n in tree.body:
        if not isinstance(n, ast.Assign) or not isinstance(n.targets[0], ast.Name):
            continue
        name, v = n.targets[0].id, n.value
        if isinstance(v, ast.Constant) and isinstance(v.value, str):
            env[name] = v.value
        elif (isinstance(v, ast.Call) and isinstance(v.func, ast.Attribute)
              and v.func.attr == "join"):
            parts = []
            for a in v.args:
                if isinstance(a, ast.Constant) and isinstance(a.value, str):
                    parts.append(a.value)
                elif isinstance(a, ast.Name) and a.id in env:
                    parts.append(env[a.id])
                else:
                    parts = None
                    break
            if parts:
                env[name] = os.path.join(*parts)
    return env


def literal(node, env):
    """The string a table element denotes, or None when it cannot be resolved statically."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.Name):
        return env.get(node.id)
    if isinstance(node, ast.BinOp) and isinstance(node.op, ast.Add):
        a, b = literal(node.left, env), literal(node.right, env)
        return None if a is None or b is None else a + b
    return None


def census():
    """Return (rows, readable_scripts, uncovered_scripts, walked).

    A row is (script_name, target_path, anchor). Only rows whose target file RESOLVES are
    returned; everything else is counted as uncovered and named, never silently dropped.
    """
    rows, readable, uncovered, walked = [], set(), [], 0
    for p in sorted(SCRIPTS.glob("*.py")):
        try:
            tree = ast.parse(p.read_text())
        except SyntaxError:
            uncovered.append((p.name, "does not parse"))
            continue
        walked += 1
        env = const_env(tree)
        found = 0
        for n in tree.body:
            if not (isinstance(n, ast.Assign) and isinstance(n.value, (ast.List, ast.Tuple))):
                continue
            elts = n.value.elts
            if not (elts and all(isinstance(e, ast.Tuple) and len(e.elts) >= 3 for e in elts)):
                continue
            for e in elts:
                vals = [literal(x, env) for x in e.elts]
                pi = next((i for i, v in enumerate(vals) if v and os.path.isfile(v)), None)
                if pi is None or pi + 1 >= len(vals) or vals[pi + 1] is None:
                    continue
                rows.append((p.name, vals[pi], vals[pi + 1]))
                found += 1
        if found:
            readable.add(p.name)
        else:
            uncovered.append((p.name, "no table row carries a resolvable target file"))
    return rows, readable, uncovered, walked


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--ci", action="store_true", help="terse output for a CI log")
    args = ap.parse_args()

    rows, readable, uncovered, walked = census()

    failures = []
    for script, target, anchor in rows:
        try:
            n = pathlib.Path(target).read_text().count(anchor)
        except OSError as e:
            failures.append((script, target, anchor, "unreadable: %s" % e))
            continue
        if n != 1:
            failures.append((script, target, anchor,
                             "anchor appears %d times, want 1" % n))

    # ⚠ THE DENOMINATOR IS THE HEADLINE, NOT A FOOTNOTE, AND IT PRINTS ON A GREEN RUN TOO.
    print("control-anchors: %d scripts walked · %d READABLE (%d anchors checked) · %d NOT COVERED"
          % (walked, len(readable), len(rows), len(uncovered)))
    print("control-anchors: ⚠ THIS READS %d OF %d SCRIPTS — a green here is a statement about "
          "those %d, NOT about this repo's anchors." % (len(readable), walked, len(readable)))

    if not args.ci:
        print("\nREADABLE:")
        for s in sorted(readable):
            print("  %-58s %d anchors" % (s, sum(1 for r in rows if r[0] == s)))
        print("\nNOT COVERED (named, never silently dropped):")
        for name, why in uncovered:
            print("  %-58s %s" % (name, why))
        print()

    # Floors. Each is checked and reported separately: a union floor is satisfied by either half.
    floor_fail = []
    if walked < WALK_FLOOR:
        floor_fail.append("VACUITY: walked %d scripts, floor %d — the walk found almost nothing, "
                          "and a walk that finds nothing reports every anchor healthy"
                          % (walked, WALK_FLOOR))
    if len(readable) < READABLE_FLOOR:
        floor_fail.append("COVERAGE: %d readable scripts, floor %d — a script drifted out of the "
                          "readable shape and coverage shrank in silence"
                          % (len(readable), READABLE_FLOOR))
    if len(rows) < ROW_FLOOR:
        floor_fail.append("COVERAGE: %d anchors checked, floor %d — rows disappeared from a table "
                          "this census could previously read" % (len(rows), ROW_FLOOR))

    for f in floor_fail:
        print("control-anchors: !! %s" % f)
    for script, target, anchor, why in failures:
        print("control-anchors: !! %s → %s: %s\n      anchor: %r"
              % (script, os.path.relpath(target, ROOT), why, anchor[:90]))

    if failures or floor_fail:
        print("\ncontrol-anchors: FAIL — %d stale anchor(s), %d floor breach(es)."
              % (len(failures), len(floor_fail)))
        if failures:
            print("  A `0 times` means the subject MOVED and that control has been measuring "
                  "nothing. A `2 times` means the literal MULTIPLIED — make the anchor unique, and "
                  "do NOT simply raise the expected count, which stops distinguishing the sites.")
        return 1
    print("control-anchors: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
