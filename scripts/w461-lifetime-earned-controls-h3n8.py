#!/usr/bin/env python3
"""W4.6.1 step 7 — positive controls for the lifetime_earned measurement.

Both measurement tests PASS on main, because they DESCRIBE main. That makes them the dangerous kind
of green: a test that asserts the status quo is indistinguishable from a test that asserts nothing
until you show it can fail. Every case below MUTATES THE PRODUCTION LEDGER toward the repair and
requires the named marker to fire — so each assertion is pinned to the defect it claims to measure,
not merely to "something changed".

The N-series simulates the FIX (a type filter on `earned +=`). The V-series attacks the harness
itself, because a measurement whose fixture can go empty proves nothing either way.

Needs LENS_TEST_DATABASE_URL. Every mutation is restored and sha256-verified: internal/mining's
ledger is the repo's money kernel and a control that leaves it mutated is worse than no control.
"""
import hashlib
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
LEDGER = ROOT / "internal" / "mining" / "cache_mining.go"
TEST = ROOT / "internal" / "earnings" / "lifetime_earned_realpg_test.go"
CLASSIFY = ROOT / "internal" / "earnings" / "classify.go"
CENSUS = ROOT / "internal" / "earnings" / "classification_census_test.go"


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(pattern="TestMeasured"):
    r = subprocess.run(
        ["go", "test", "-count=1", "-run", pattern, "./internal/earnings/"],
        cwd=ROOT, capture_output=True, text=True,
    )
    return r.returncode, r.stdout + r.stderr


# The one line the whole finding rests on.
EARNED_LINE = "\t\tdelta = amount\n\t\tearned += amount"

CASES = [
    ("N1", LEDGER, "THE REPAIR: `earned +=` filtered to counted-supply mint types only",
     [(EARNED_LINE,
       "\t\tdelta = amount\n\t\tif isCountedSupplyTypeForControl(txType) {\n\t\t\tearned += amount\n\t\t}"),
      ("func (s *LedgerStore) apply(",
       "func isCountedSupplyTypeForControl(t string) bool {\n\tfor _, c := range countedSupplyTypeList {\n\t\tif c == t {\n\t\t\treturn true\n\t\t}\n\t}\n\treturn false\n}\n\nfunc (s *LedgerStore) apply(")],
     "[W461-CREDITED]", "TestMeasured"),

    ("N2", LEDGER, "a narrower repair: only `unstake` stops counting as earned",
     [(EARNED_LINE,
       "\t\tdelta = amount\n\t\tif txType != \"unstake\" {\n\t\t\tearned += amount\n\t\t}")],
     "[W461-RT]", "TestMeasured"),

    ("N3", LEDGER, "lifetime_earned stops moving on credits altogether",
     [(EARNED_LINE, "\t\tdelta = amount")],
     "[W461-INFLATED]", "TestMeasured"),

    # ── V-series: the harness's own failure modes ──
    ("V1", TEST, "VACUITY: the ledger sum reads a workspace with no rows",
     [('`SELECT type, COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 GROUP BY type`, ws)',
       '`SELECT type, COALESCE(SUM(amount),0) FROM lens_token_ledger WHERE workspace_id=$1 GROUP BY type`, ws+"-nonexistent")')],
     "[HARNESS]", "TestMeasured"),

    ("V2", CLASSIFY, "the classification stops recognising the one genuine earning",
     [('"cache_mine":                {Settled, Contribution,',
       '"cache_mine":                {NotEarnings, NotIncome,')],
     "[W461-CLASSIFY]", "TestMeasured"),

    ("V3", TEST, "the round trip is no longer value-neutral, so its conclusion would not follow",
     [('\t\t\treturn ls.DebitTx(ctx, tx, ws, principal, "stake", "LENS staked for yield", nil)',
       '\t\t\treturn ls.DebitTx(ctx, tx, ws, principal/2, "stake", "LENS staked for yield", nil)')],
     "[W461-RT-BALANCE]", "TestMeasured"),

    # ── E-series: the classification census's own failure modes ──
    ("E-C1", CLASSIFY, "a mint type is dropped from the vocabulary, the normal way it goes stale",
     [('\t"pool_royalty":              {Settled, Contribution,', '\t"pool_royalty_UNUSED":       {Settled, Contribution,')],
     "[E1]", "TestE"),

    ("E-C2", CENSUS, "VACUITY: the census parses a package with no ledger-type constants",
     [('for _, dir := range []string{filepath.Join("..", "mining"), filepath.Join("..", "povi")}',
       'for _, dir := range []string{filepath.Join("..", "envelope")}')],
     "[E1-", "TestE"),

    ("E-C3", CLASSIFY, "one of internal/economy's BARE LITERAL types loses its rule",
     [('\t"marketplace_refund":        {NotEarnings, NotIncome,', '\t"marketplace_refund_X":      {NotEarnings, NotIncome,')],
     "[E2]", "TestE"),

    ("E-C4", CLASSIFY, "a rule keeps its verdict and loses its argument",
     [('"the workspace spending its own LENS"', '"no"')],
     "[E3]", "TestE"),

    ("E-C5", CLASSIFY, "staking yield is folded into contribution — the sentence-breaking error",
     [('\t"stake_yield": {Settled, Capital,', '\t"stake_yield": {Settled, Contribution,')],
     "[E4-KIND]", "TestE"),
]


def main():
    if not os.environ.get("LENS_TEST_DATABASE_URL"):
        print("LENS_TEST_DATABASE_URL is not set — the measurement SKIPS, and a control over a "
              "skipped test proves nothing. Refusing to report a pass.")
        return 2

    before = {p: sha(p) for p in (LEDGER, TEST, CLASSIFY, CENSUS)}

    code, out = run()
    if code != 0:
        print("BASELINE IS NOT GREEN:\n" + out)
        return 1
    if "MEASURED:" not in out and "-v" not in out:
        pass
    print("baseline: both measurements GREEN on main (they describe main)\n")

    caught, missed = [], []
    for cid, target, why, edits, marker, pattern in CASES:
        original = target.read_text()
        if not all(original.count(o) == 1 for o, _ in edits):
            print(f"{cid} ANCHOR MISS: sites occur {[original.count(o) for o, _ in edits]}, want all 1 — "
                  f"the control never ran. ({why})")
            missed.append(cid)
            continue
        try:
            mutated = original
            for o, n in edits:
                mutated = mutated.replace(o, n, 1)
            target.write_text(mutated)
            code, out = run(pattern)
            if code == 0:
                print(f"{cid} NOT CAUGHT — {why}: still GREEN. The assertion does not depend on the "
                      f"behaviour it names.")
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

    code, out = run()
    for f in before:
        if sha(f) != before[f]:
            print(f"RESTORE DRIFT on {f}")
            return 2
    print(f"\nrestored: sha256 matches for all {len(before)} touched files; re-run "
          f"{'GREEN' if code == 0 else 'RED'}")
    print(f"CONTROLS: {len(caught)}/{len(CASES)} CAUGHT" + (f"; MISSED {missed}" if missed else ""))
    return 0 if not missed and code == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
