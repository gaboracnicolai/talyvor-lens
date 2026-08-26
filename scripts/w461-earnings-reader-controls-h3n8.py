#!/usr/bin/env python3
"""W4.6.1 step 7 — positive controls for the earnings READER and its route wiring.

Every one of these guards passed on its first run. This mutates the reader, the classification and
main.go's wiring one property at a time and requires the NAMED marker to fire, so each assertion is
pinned to the behaviour it claims to test rather than to "something is broken".

Needs LENS_TEST_DATABASE_URL for the R-series; the script refuses to report a pass without it,
because a control over a skipped test proves nothing. All mutations are sha256-verified on restore.
"""
import hashlib
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
READER = ROOT / "internal" / "earnings" / "reader.go"
CLASSIFY = ROOT / "internal" / "earnings" / "classify.go"
MAIN = ROOT / "cmd" / "lens" / "main.go"
RTEST = ROOT / "internal" / "earnings" / "reader_realpg_test.go"


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(pkg, pattern):
    r = subprocess.run(["go", "test", "-count=1", "-run", pattern, pkg],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode, r.stdout + r.stderr


CASES = [
    ("R-C1", READER, "capital (stake yield) is folded into the contribution subtotal — the error that "
                     "makes the SENTENCE false while the total stays right",
     [("\t\t\tif rule.Kind == Capital {\n\t\t\t\ts.CapitalSettledULENS += total\n\t\t\t} else {\n\t\t\t\ts.ContributionSettledULENS += total\n\t\t\t}",
       "\t\t\ts.ContributionSettledULENS += total")],
     "[R1-CONTRIB]", "./internal/earnings/", "TestR1"),

    ("R-C2", READER, "the by-type query drops its workspace scope",
     [("\tWHERE workspace_id = $1\n\tGROUP BY type", "\tWHERE $1 = $1\n\tGROUP BY type")],
     "[R2]", "./internal/earnings/", "TestR2"),

    ("R-C3", READER, "THE TRAP: held is summed from `*_held` LEDGER ROWS instead of the balance column",
     [('const heldSQL = `SELECT held_balance::bigint FROM lens_token_balances WHERE workspace_id = $1`',
       "const heldSQL = `SELECT COALESCE(SUM(amount),0)::bigint FROM lens_token_ledger WHERE workspace_id = $1 AND type LIKE '%_held'`")],
     "[R3]", "./internal/earnings/", "TestR3"),

    ("R-C4", READER, "earning_enabled ignores the gates and always says yes",
     [("\t\tEarningEnabled: gates.EconomyEnabled && gates.PoolRoyaltyMintingEnabled && (gates.CachePoolableEnabled || gates.DistillPoolableEnabled),",
       "\t\tEarningEnabled: true,")],
     "[R4-OFF]", "./internal/earnings/", "TestR4"),

    ("R-C5", READER, "an unclassified ledger type is silently dropped instead of reported",
     [("\t\tcase Unclassified:\n\t\t\ts.UnclassifiedTypes = append(s.UnclassifiedTypes, typ)",
       "\t\tcase Unclassified:")],
     "[R5]", "./internal/earnings/", "TestR5"),

    ("R-C6", READER, "an unclassified type is COUNTED as contribution — worse than dropping it",
     [("\t\tcase Unclassified:\n\t\t\ts.UnclassifiedTypes = append(s.UnclassifiedTypes, typ)",
       "\t\tcase Unclassified:\n\t\t\ts.UnclassifiedTypes = append(s.UnclassifiedTypes, typ)\n\t\t\ts.ContributionSettledULENS += total")],
     "[R5-COUNTED]", "./internal/earnings/", "TestR5"),

    ("R-C7", READER, "the USD conversion forgets the peg and reports LENS as dollars",
     [("\treturn (float64(micro) / 1_000_000.0) / economy.LENSPerUSD",
       "\treturn float64(micro) / 1_000_000.0")],
     "[R1-USD]", "./internal/earnings/", "TestR1"),

    ("R-C8", CLASSIFY, "a not-income type (unstake) is reclassified as settled contribution",
     [('\t"unstake":                   {NotEarnings, NotIncome,', '\t"unstake":                   {Settled, Contribution,')],
     "[R1-CONTRIB]", "./internal/earnings/", "TestR1"),

    # ── the wiring guards ──
    # ⚠ THE FIRST VERSION OF THIS CONTROL USED `pub` AND DID NOT COMPILE — it went red with no
    # marker at all. A control that cannot build proves the COMPILER noticed, not the test. `r` is
    # the enclosing router and is in scope inside the group closure, so this mutation builds and the
    # guard has to earn its red.
    ("EW-C1", MAIN, "the route is moved off the isolated group onto the enclosing router",
     [('econ.get(authed, "/v1/workspaces/{wsID}/earnings"', 'econ.get(r, "/v1/workspaces/{wsID}/earnings"')],
     "[EW2]", "./cmd/lens/", "TestEarningsRoute"),

    ("EW-C2", MAIN, "a gate is hardcoded true instead of read from config — the debugging literal",
     [("\t\t\t\tEconomyEnabled:            cfg.EconomyEnabled,", "\t\t\t\tEconomyEnabled:            true,")],
     "[EW5]", "./cmd/lens/", "TestEarningsRoute"),

    ("EW-C3", MAIN, "a gate is dropped from the literal, so it silently defaults to off",
     [("\t\t\t\tDistillPoolableEnabled:    cfg.DistillPoolableEnabled,\n", "")],
     "[EW6]", "./cmd/lens/", "TestEarningsRoute"),

    # ── vacuity on the harness itself ──
    ("V-C1", RTEST, "VACUITY: R3 compares held against a rows-sum that is also the column",
     [("`SELECT COALESCE(SUM(amount),0)::bigint FROM lens_token_ledger\n\t\t WHERE workspace_id=$1 AND type='pool_royalty_held'`",
       "`SELECT held_balance::bigint FROM lens_token_balances WHERE workspace_id=$1`")],
     "[R3-PREMISE]", "./internal/earnings/", "TestR3"),
]


def main():
    if not os.environ.get("LENS_TEST_DATABASE_URL"):
        print("LENS_TEST_DATABASE_URL is not set — the R-series SKIPS and a control over a skipped "
              "test proves nothing. Refusing to report a pass.")
        return 2

    before = {p: sha(p) for p in (READER, CLASSIFY, MAIN, RTEST)}
    for pkg, pat in (("./internal/earnings/", "TestR"), ("./cmd/lens/", "TestEarningsRoute")):
        code, out = run(pkg, pat)
        if code != 0:
            print(f"BASELINE NOT GREEN for {pkg} {pat}:\n{out}")
            return 1
    print("baseline: reader + wiring guards GREEN\n")

    caught, missed = [], []
    for cid, target, why, edits, marker, pkg, pat in CASES:
        original = target.read_text()
        if not all(original.count(o) == 1 for o, _ in edits):
            print(f"{cid} ANCHOR MISS: sites occur {[original.count(o) for o, _ in edits]}, want all 1 "
                  f"— the control never ran. ({why})")
            missed.append(cid)
            continue
        try:
            mutated = original
            for o, n in edits:
                mutated = mutated.replace(o, n, 1)
            target.write_text(mutated)
            code, out = run(pkg, pat)
            if code == 0:
                print(f"{cid} NOT CAUGHT — {why}: still GREEN.")
                missed.append(cid)
            elif marker not in out:
                tags = sorted(set(re.findall(r"\[[A-Z0-9-]+\]", out)))
                print(f"{cid} WRONG GUARD — {why}: red, but {marker} never fired. Saw {tags}.")
                missed.append(cid)
            else:
                print(f"{cid} CAUGHT by {marker} — {why}")
                caught.append(cid)
        finally:
            target.write_text(original)
            if sha(target) != before[target]:
                print(f"{cid} RESTORE FAILED on {target} — STOPPING.")
                return 2

    for f in before:
        if sha(f) != before[f]:
            print(f"RESTORE DRIFT on {f}")
            return 2
    ok = True
    for pkg, pat in (("./internal/earnings/", "TestR"), ("./cmd/lens/", "TestEarningsRoute")):
        code, _ = run(pkg, pat)
        ok = ok and code == 0
    print(f"\nrestored: sha256 matches for all {len(before)} touched files; re-run "
          f"{'GREEN' if ok else 'RED'}")
    print(f"CONTROLS: {len(caught)}/{len(CASES)} CAUGHT" + (f"; MISSED {missed}" if missed else ""))
    return 0 if not missed and ok else 1


if __name__ == "__main__":
    sys.exit(main())
