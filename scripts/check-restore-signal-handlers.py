#!/usr/bin/env python3
"""Every control script that mutates a tracked file and restores it in a `finally` must also
install a restoring SIGNAL HANDLER — because a `finally` does not run on SIGTERM.

WHY THIS EXISTS, MEASURED RATHER THAN SUPPOSED — AND IT WAS MEASURED IN talyvor-suite, NOT HERE,
WHICH IS THE POINT OF PORTING IT. A 2-minute command timeout SIGTERM'd a control script mid-control
(talyvor-suite W1.7, merge 78c69c8). The `finally` never ran and the working tree was left with
`deploy/decision-expiry.sh` reading `if true; then` where a gate had been. It was reproduced on
demand in talyvor-suite `5de27e3`: same kill, same file — with a handler nothing was stranded,
without one the mutated file was left in the tree.

⚠ NOTHING IN THIS REPOSITORY HAD EVER LOOKED FOR IT, AND IT IS THE LAST ONE TO GET A GUARD AT ALL.
At 16ac8c3, 16 of this repo's scripts mutate-and-restore and ZERO installed a handler. talyvor-suite
(5de27e3 -> 4829830), talyvor-docs (ffe9063 -> fe12622), talyvor-track (5f01947) and talyvor-code
(bfa11f4) were all guarded first; this closes the estate.

⚠ NOTHING ABOUT THAT TREE LOOKED WRONG. `pnpm test` had passed minutes earlier, `git status`
showed only files the session had edited on purpose, and the diff was one line inside a table that
had legitimately been touched. It was found only because the NEXT harness refused to score against
a red baseline. The stranded state is the dangerous kind: a kill landing on a control whose
mutation WEAKENS a guard leaves that weakening in the tree with a green suite.

⚠⚠ AND THIS IS A GUARD RATHER THAN A BATCH OF EDITS BECAUSE A POPULATION NAMED IN PROSE DOES NOT
STOP GROWING — measured in talyvor-suite, where the fix sat in one file while the directory grew by
twelve scripts and the unprotected count went UP. The allowlist below may only SHRINK: R2 fails on
an entry that has been fixed, so the list cannot rot into a permanent excuse, and R1 fails on a NEW
unprotected mutator, so the count cannot climb.

⚠ DETECTION IS SYNTACTIC, NOT A GREP, AND THAT IS LOAD-BEARING HERE RATHER THAN STYLISTIC.
In talyvor-suite `grep -l signal scripts/*.py` reports a script as PROTECTED whose only
occurrences of the word are four sentences of English in comments. A regex reads the documentation
as the implementation; `ast` does not see comments.

⚠ AND THE TWO POPULATIONS DIFFER HERE TOO, MEASURED ON THIS TREE AT f8e7236: `grep -l finally:
scripts/*.py` counts 60, this walk counts 43. A `finally` that does not WRITE is not a restore, and
seventeen scripts in this directory have one.

WHAT COUNTS AS A MUTATOR: a script containing a `try` whose `finally` performs a write —
`.write_text` / `.write_bytes` / `.writelines` / `.write`, or `shutil`/`os` copy/move. That is the
restore, and a script that restores is a script that mutated.

WHAT COUNTS AS PROTECTED: a call to `signal.signal(...)`. This does NOT verify the handler
restores correctly or re-raises; it verifies one is installed. Converting a script needs its own
SIGTERM control — kill it mid-mutation with the handler, and again without it, and require the
second to strand what the first restores. A handler nobody has watched work is the same thing as
no handler.

THE SHAPE TO COPY IS BELOW rather than a pointer into another repository, because a cross-repo
pointer is a claim nothing in this repo can check:

    def restore_on_signal(snapshot: dict) -> None:
        def handler(signum, _frame):
            for path, blob in snapshot.items():
                try:
                    path.write_bytes(blob)
                except OSError:
                    pass
            sys.stderr.write("\n!! signal %d — restored %d file(s)\n" % (signum, len(snapshot)))
            signal.signal(signum, signal.SIG_DFL)
            os.kill(os.getpid(), signum)
        for s in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            signal.signal(s, handler)

Install it AFTER the snapshot exists, and re-install it if the snapshot changes. Re-raising with
SIG_DFL keeps the exit status honest: a caller that killed the process still sees it die of that
signal rather than exit 0 with a tidy tree. SIGKILL still strands and nothing in Python can change
that; the defence there is a harness that refuses to score against a red baseline.

Usage:  python3 scripts/check-restore-signal-handlers.py
Exit 0 = every mutator is protected or listed; non-zero names what changed.
"""
import ast
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS = REPO / "scripts"

# ⚠⚠ ONE FLOOR PER DETECTOR, AND THIS IS THE MOST IMPORTANT LINE IN THE FILE.
#
# A SINGLE FLOOR OVER THE UNION OF TWO DETECTORS IS SATISFIED BY EITHER DETECTOR ALONE. Measured
# by tab-r7k2 in bfa11f4: stubbing the WIDE detector to `return False` came back GREEN, because
# the narrow one held the union count up — so the widened definition, the entire point of having
# two, could have silently reverted with the guard green. A vacuity floor over a union is not a
# vacuity floor. Controls G10/G11 blind each detector separately for exactly this reason.
#
# Both are FLOORS ON THE DETECTOR, not targets for the tree: a walk that stops recognising its
# shape reports a clean directory. Deleting scripts is legitimate — lower the floor in the same
# diff, with the deletions visible.
NARROW_FLOOR = 13   # 16 restore-in-`finally` at 16ac8c3 (direct, indirect, context-manager, git)
WIDE_FLOOR = 7     # 10 read-and-write scripts at 16ac8c3

# The scripts that mutate-and-restore and do NOT yet install a handler, measured at 16ac8c3.
# ⚠ THIS LIST MAY ONLY SHRINK. Adding to it is not a fix; R1 exists so that a new script cannot be
# written without a handler, and R2 exists so that a fixed script cannot be left listed.
UNPROTECTED = {
    "w191-rotation-controls-h3n8.py",
    "w22-gate-controls.py",
    "w22-pooled-write-controls.py",
    "w22-threshold-controls.py",
    "w461-allowance-controls-q4vn.py",
    "w461-earnings-reader-controls-h3n8.py",
    "w461-lifetime-earned-controls-h3n8.py",
    "w461-subscription-controls-q4vn.py",
    "w59-runbook-link-controls.py",
    "w610-envelope-controls-h3n8.py",
    "w626-tenant-erasure-population-controls.py",
    "w627-tenant-config-enforcement-controls.py",
    "w628-tare-conformance-controls.py",
    "w629-openapi-route-contract-controls.py",
    "w630-error-contract-controls.py",
}

# HAPPY_PATH_ONLY holds the scripts the WIDE detector found that restore WITHOUT a `try` at all.
# They are a WORSE failure than an entry in UNPROTECTED, not a lesser one: an unprotected `finally`
# strands only on a signal, these strand on ANY exception. Measured at 16ac8c3 — ZERO of them here,
# once all four restore idioms are resolved. The empty list is a RESULT, not an absence of looking:
# R6 fires on the next one that appears.
#
# ⚠ THIS LIST MAY ONLY SHRINK, and it is kept SEPARATE from UNPROTECTED deliberately: folding the
# two would let a script leave the harder list by acquiring a handler while still having no
# `finally`. R2b fails on an entry that has gained one; R4b on one the detector no longer finds.
HAPPY_PATH_ONLY: set[str] = set()

# NOT_MUTATORS holds scripts the WIDE detector catches that do not actually restore a tracked
# file — a wide net manufactures false positives, and parking one here without a REASON is a hole
# rather than a classification. R7 fails on an unexplained entry; R4 fails on one that is no
# longer a candidate. It is empty today: every one of the 11 the wide net found here is a genuine
# happy-path restore, verified by reading them.
NOT_MUTATORS: dict[str, str] = {}

_WRITE_ATTRS = {"write_text", "write_bytes", "writelines", "write"}
_COPY_FUNCS = {"copy", "copy2", "copyfile", "move"}
_READ_ATTRS = {"read_text", "read_bytes"}


def _write_call(n: ast.AST) -> bool:
    if not (isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)):
        return False
    if n.func.attr in _WRITE_ATTRS:
        return True
    return (n.func.attr in _COPY_FUNCS and isinstance(n.func.value, ast.Name)
            and n.func.value.id in ("shutil", "os"))


def _writes(body) -> bool:
    """True if this statement list performs a filesystem write."""
    return any(_write_call(n) for n in ast.walk(ast.Module(body=body, type_ignores=[])))


def _reads_and_writes(tree: ast.AST) -> bool:
    """The WIDE net: the script both reads file content and writes it, `try` or no `try`."""
    reads = any(isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)
                and n.func.attr in _READ_ATTRS for n in ast.walk(tree))
    return reads and any(_write_call(n) for n in ast.walk(tree))


def _writing_functions(tree: ast.AST) -> set:
    """Names of functions defined in this module whose body performs a write.

    ⚠ THIS EXISTS BECAUSE THE FIRST VERSION OF THIS DETECTOR WAS WRONG ABOUT TEN OF ELEVEN
    SCRIPTS, AND THE FAILURE MESSAGE IT WOULD HAVE PRINTED WAS A FALSE STATEMENT ABOUT THEM.
    `_writes(finalbody)` looks for a write CALL syntactically inside the `finally`. These scripts
    restore INDIRECTLY — `finally: restore()`, `finally: revert(...)`, `finally: git_restore()` —
    so the walk saw a `finally` with no write in it and classified them as having no `try` at all.
    Ten of the eleven this repo's wide net first flagged are that shape; exactly one
    (in talyvor-suite, w11-spa-cache-controls.py) genuinely had no `try`; here there are three.

    One level of indirection is deliberately all this resolves. It is a syntactic guard, not an
    interpreter, and a restore reached through two hops would be reported as happy-path — wrongly,
    but LOUDLY and in the direction that asks a human to look, which is the safe direction here.
    """
    out = set()
    for n in ast.walk(tree):
        if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and _writes(n.body):
            out.add(n.name)
    return out


def _restores_via_context_manager(tree: ast.AST) -> bool:
    """A class whose `__exit__` writes, and a `with` that could be using it.

    ⚠ A CONTEXT MANAGER IS AS EXCEPTION-SAFE AS A `finally` — that is what `with` is for — so
    calling one "restores on the happy path only" is a false statement about the script. This is
    the THIRD classification the population needed: keyed on a write inside `finally`, a restore
    reached through a helper function was invisible; keyed on that too, a restore reached through
    `__exit__` still was. In talyvor-docs, w31-viewguard-skip-controls-7e2b.py's own docstring says
    "Every
    mutation is restored in a `finally`", and it is right — the `finally` is the `with` statement.

    BOTH halves are required: defining the class is not using it. A script that defines a writing
    `__exit__` and never writes a `with` is still reported, which is the loud direction.
    """
    has_exit = any(isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))
                   and n.name == "__exit__" and _writes(n.body) for n in ast.walk(tree))
    return has_exit and any(isinstance(n, (ast.With, ast.AsyncWith)) for n in ast.walk(tree))


def _restores_via_git_in_finally(node: ast.Try) -> bool:
    """A `finally` that shells out to `git checkout -- <path>`.

    ⚠ THE FOURTH RESTORE IDIOM THIS POPULATION USES, AND THE FOURTH TIME THE COUNT MOVED.
    A subprocess is not a Python write, so a `finally` that restores with git looked empty to the
    walk. In talyvor-docs BOTH remaining candidates were that shape, taking its genuinely-
    happy-path count to zero; in THIS repo the single candidate was a context manager, so the count
    here is zero too. Estate-wide it has gone 27 -> 13 (indirect calls) -> 10 (context managers)
    -> 6 (this).

    ⚠ THE PREDICATE IS DELIBERATELY TIGHT: a string literal containing "git" AND one containing
    "checkout" or "restore", in the same call. A `finally` that merely runs some subprocess is NOT
    a restore, and treating it as one would make this guard quieter than the truth.

    ⚠ AND IT DOES NOT MAKE THE SCRIPT SAFE — it makes it EXCEPTION-safe. `git checkout` in a
    `finally` still does not run on SIGTERM, so these scripts move from R6 to R1: they need a
    handler, exactly like the rest.
    """
    for c in ast.walk(ast.Module(body=node.finalbody, type_ignores=[])):
        if not isinstance(c, ast.Call):
            continue
        lits = [x.value for x in ast.walk(c)
                if isinstance(x, ast.Constant) and isinstance(x.value, str)]
        if any("git" in s for s in lits) and any(("checkout" in s or "restore" in s) for s in lits):
            return True
    return False


def _restores_in_finally(tree: ast.AST) -> bool:
    if _restores_via_context_manager(tree):
        return True
    for n in ast.walk(tree):
        if isinstance(n, ast.Try) and n.finalbody and _restores_via_git_in_finally(n):
            return True
    writers = _writing_functions(tree)
    for n in ast.walk(tree):
        if not (isinstance(n, ast.Try) and n.finalbody):
            continue
        if _writes(n.finalbody):
            return True
        # INDIRECT: the `finally` calls a function defined here that writes.
        for c in ast.walk(ast.Module(body=n.finalbody, type_ignores=[])):
            if isinstance(c, ast.Call):
                f = c.func
                name = f.id if isinstance(f, ast.Name) else (f.attr if isinstance(f, ast.Attribute) else None)
                if name in writers:
                    return True
    return False


def _installs_handler(tree: ast.AST) -> bool:
    """A call to `signal.signal(...)`. Comments are invisible to ast, which is the point."""
    return any(
        isinstance(n, ast.Call) and isinstance(n.func, ast.Attribute)
        and n.func.attr == "signal"
        and isinstance(n.func.value, ast.Name) and n.func.value.id == "signal"
        for n in ast.walk(tree)
    )


def main() -> int:
    narrow, wide, protected, errors = set(), set(), set(), []
    for path in sorted(SCRIPTS.glob("*.py")):
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"))
        except (SyntaxError, UnicodeDecodeError) as e:
            # R5: a file this guard cannot read is reported, never skipped. A walk that silently
            # drops what it cannot parse reports a clean directory.
            errors.append(f"R5: {path.name} could not be parsed, so it was NOT checked: {e}")
            continue
        if _restores_in_finally(tree):
            narrow.add(path.name)
        if _reads_and_writes(tree):
            wide.add(path.name)
        if (path.name in narrow or path.name in wide) and _installs_handler(tree):
            protected.add(path.name)

    fail = list(errors)

    # R3 FLOORS FIRST, PER DETECTOR, because every rule below is vacuous without a population and
    # a union floor would let either detector die unnoticed.
    if len(narrow) < NARROW_FLOOR:
        fail.append(
            f"R3: the `finally`-restore detector found only {len(narrow)} scripts, floor is "
            f"{NARROW_FLOOR}. A detector that stops recognising its shape reports a clean "
            "directory rather than a broken instrument.")
    if len(wide) < WIDE_FLOOR:
        fail.append(
            f"R3b: the read-and-write detector found only {len(wide)} scripts, floor is "
            f"{WIDE_FLOOR}. This floor is SEPARATE from the one above on purpose: a single floor "
            "over the union is satisfied by either detector alone, so the wide half could revert "
            "to silence with the guard green.")

    # R1: a `finally`-restoring script with no handler that nobody has accounted for.
    for name in sorted(narrow - protected - UNPROTECTED):
        fail.append(
            f"R1: {name} mutates a tracked file and restores it in a `finally`, but installs no "
            "signal handler — a SIGTERM will strand the mutation in the working tree. The shape "
            "to copy is in this file's own docstring, and the conversion needs its own SIGTERM control.")

    # R6: restoring only on the HAPPY PATH is worse than an unprotected `finally`, not better —
    # it strands the tree on any exception, not merely on a signal.
    for name in sorted(wide - narrow - NOT_MUTATORS.keys() - HAPPY_PATH_ONLY):
        fail.append(
            f"R6: {name} reads and writes tracked files, and this walk could NOT find a restore "
            "in a `finally` — directly, through a helper it defines, through a `with`/__exit__, or "
            "through a `git checkout`. Either there is none (any exception strands the mutation), "
            "or the restore is reached in a way a syntactic walk cannot follow — two hops of "
            "indirection, or an imported helper. READ IT BEFORE CONVERTING: the count has already "
            "moved four times (27 -> 13 -> 10 -> 6 estate-wide) because each idiom was invisible "
            "until somebody tried to convert one. If it does restore, classify it in NOT_MUTATORS "
            "with the reason; if it does not, wrap it in a `finally` AND install a handler.")

    # R2: an entry that has been FIXED must leave the list, or the list rots into an excuse.
    for name in sorted(UNPROTECTED & protected):
        fail.append(
            f"R2: {name} now installs a handler but is still listed in UNPROTECTED. Remove the "
            "entry — this list may only shrink.")
    for name in sorted(HAPPY_PATH_ONLY & narrow):
        fail.append(
            f"R2b: {name} now restores in a `finally` but is still listed in HAPPY_PATH_ONLY. "
            "Remove the entry — this list may only shrink.")

    # R4: an entry that is no longer a candidate (deleted, renamed, restructured) is stale.
    for name in sorted(UNPROTECTED - narrow):
        fail.append(
            f"R4: UNPROTECTED lists {name}, which is not a `finally`-restoring script here. "
            "Remove the entry.")
    for name in sorted(HAPPY_PATH_ONLY - wide):
        fail.append(
            f"R4b: HAPPY_PATH_ONLY lists {name}, which the read-and-write detector no longer "
            "finds. Remove the entry.")
    for name in sorted(set(NOT_MUTATORS) - wide):
        fail.append(
            f"R4c: NOT_MUTATORS lists {name}, which the read-and-write detector no longer finds. "
            "Remove the entry.")

    # R7: an unexplained exemption is a hole, not a classification.
    for name, why in sorted(NOT_MUTATORS.items()):
        if not why.strip():
            fail.append(
                f"R7: NOT_MUTATORS lists {name} with no reason. An entry nobody had to justify is "
                "a hole in the population, not a classification.")

    print(f"restore-signal-handlers: {len(narrow)} restore-in-`finally`, {len(wide)} read-and-write, "
          f"{len(protected)} protected; {len(UNPROTECTED)} awaiting a handler, "
          f"{len(HAPPY_PATH_ONLY)} awaiting a `finally`, {len(NOT_MUTATORS)} classified not-mutators")
    if fail:
        ci = len(sys.argv) > 1 and sys.argv[1] == "--ci"
        for line in fail:
            print(f"::error::{line}" if ci else line)
        return 1
    print("restore-signal-handlers: ok")
    return 0


sys.exit(main())
