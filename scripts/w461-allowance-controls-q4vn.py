#!/usr/bin/env python3
"""
POSITIVE CONTROLS for W4.6.1 step 2 — the allowance ledger. tab-q4vn.

⚠ THE PROPERTY UNDER TEST IS A BOUND: "worst case per subscriber is EXACTLY D". Every
test in allowance_integration_test.go passed on its first run, and a bound that has
never been seen to fail is a bound nobody has tested — especially the concurrency one,
where the naive implementation passes single-threaded and leaks under load.

⚠ A1 IS THE ONE THAT MATTERS. It removes the row lock, so two concurrent settles can
read the same remaining and both spend it. If A1 does not turn the concurrent test RED,
that test is theatre: it would pass over an implementation that lets D leak.

Convention as w11-display-sweep-controls.py: anchor count asserted before the write,
bytes verified changed, a MUST-RED target AND a MUST-STAY-GREEN companion, byte-identical
sha256-verified restore. Refuses to run without a real database rather than scoring
itself over skipped tests.
"""

import hashlib
import os
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
ALLOW = REPO / "internal/billing/allowance.go"
SUBS = REPO / "internal/billing/subscriptions.go"

T_CONC = "TestAllowance_HardCap_ConcurrentConsumesCannotExceedD"
T_SINGLE = "TestAllowance_HardCap_SingleOverspendIsClamped"
T_MANY = "TestAllowance_HardCap_ManySmallConsumesCannotExceedD"
T_IDEM = "TestAllowance_GrantIsIdempotentPerPeriod"
T_WITHIN = "TestAllowance_GrantThenConsume_WithinD"
T_STALE = "TestAllowance_StaleEventGrantsNothing"
T_OFF = "TestAllowance_NotConfigured_GrantsNothingAndCoversNothing"
T_E2E = "TestAllowance_SubscriptionCreatedGrantsThePeriod"


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(test: str) -> bool:
    r = subprocess.run(["go", "test", "./internal/billing/", "-run", "^" + test + "$", "-count=1"],
                       cwd=REPO, capture_output=True, text=True)
    if "SKIP" in r.stdout:
        print(f"      ⚠ {test} SKIPPED — no LENS_TEST_DATABASE_URL; this harness proves nothing")
        sys.exit(2)
    return r.returncode == 0


def control(cid, desc, path, old, new, must_red, must_green, expect_count=1):
    src = path.read_text(encoding="utf-8")
    n = src.count(old)
    if n != expect_count:
        print(f"  {cid}  ✗ ANCHOR DEAD — {path.name} holds it {n}×, expected {expect_count}")
        return False
    before = sha(path)
    path.write_text(src.replace(old, new, expect_count), encoding="utf-8")
    if sha(path) == before:
        print(f"  {cid}  ✗ THE WRITE CHANGED NOTHING")
        return False
    try:
        red = not run(must_red)
        green = run(must_green)
    finally:
        path.write_text(src, encoding="utf-8")
        assert sha(path) == before, f"{cid}: RESTORE FAILED on {path}"
    verdict, ok = (
        ("CAUGHT", True) if red and green
        else ("SUSPECT (companion also red — breaks the build, does not probe)", False) if red
        else ("NOT CAUGHT ⚠ THE TEST IS BLIND TO THIS", False)
    )
    print(f"  {cid}  {verdict}\n      {desc}")
    print(f"      must-red   {must_red} → {'RED' if red else 'GREEN'}")
    print(f"      must-green {must_green} → {'GREEN' if green else 'RED'}")
    return ok


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("LENS_TEST_DATABASE_URL is not set — these are real-PG tests and would SKIP.")
        print("A harness whose targets skip reports all-CAUGHT over nothing. Refusing to run.")
        return 2
    print("W4.6.1 step 2 — ALLOWANCE POSITIVE CONTROLS (tab-q4vn)\n")
    r = []

    r.append(control(
        "A1", "⚠ THE ROW LOCK IS REMOVED — two concurrent settles read the same remaining and "
              "both spend it, so D leaks under load while every single-threaded test still passes",
        ALLOW,
        "			ORDER BY period_start DESC LIMIT 1\n			FOR UPDATE\n		)",
        "			ORDER BY period_start DESC LIMIT 1\n		)",
        must_red=T_CONC, must_green=T_SINGLE,
    ))

    r.append(control(
        "A2", "the CLAMP is removed from the write — the cost is added whole, so a single "
              "overspend either exceeds D or is rejected outright by the CHECK",
        ALLOW,
        "		SET consumed_ulxc = a.consumed_ulxc + LEAST(t.remaining, $3::bigint),",
        "		SET consumed_ulxc = a.consumed_ulxc + $3::bigint,",
        must_red=T_SINGLE, must_green=T_WITHIN,
    ))

    r.append(control(
        "A3", "the clamp is removed from what is REPORTED — the caller is told the allowance "
              "covered more than it holds, which is overage in the accounting even if not in the row",
        ALLOW,
        "		RETURNING LEAST(t.remaining, $3::bigint)`,",
        "		RETURNING $3::bigint`,",
        must_red=T_SINGLE, must_green=T_WITHIN,
    ))

    r.append(control(
        "A4", "the per-period grant stops being idempotent — a redelivered renewal hands one "
              "subscriber 2D for one fee, which is exactly what the worst-case bound rules out",
        ALLOW,
        "		ON CONFLICT (stripe_subscription_id, period_start) DO NOTHING`,",
        "		`,",
        must_red=T_IDEM, must_green=T_WITHIN,
    ))

    r.append(control(
        "A5", "⚠ a STALE or REFUSED subscription event starts granting an allowance — the "
              "out-of-order bug wearing a different hat, and an expensive one",
        SUBS,
        '	if applied && (status == "active" || status == "trialing") {',
        '	if status == "active" || status == "trialing" || true {',
        must_red=T_STALE, must_green=T_E2E,
    ))

    r.append(control(
        "A6", "D=0 stops meaning 'no allowance' — a deployment that never priced the allowance "
              "starts granting one",
        ALLOW,
        "	if s.allowanceULXC <= 0 {\n		return false, ErrNoAllowanceConfigured\n	}",
        "	if s.allowanceULXC < 0 {\n		return false, ErrNoAllowanceConfigured\n	}",
        must_red=T_OFF, must_green=T_WITHIN,
    ))

    print()
    caught = sum(1 for x in r if x)
    print(f"{caught}/{len(r)} controls CAUGHT")
    print("allowance.go / subscriptions.go restored and sha256-verified inside each control")
    return 0 if caught == len(r) else 1


if __name__ == "__main__":
    sys.exit(main())
