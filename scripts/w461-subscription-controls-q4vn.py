#!/usr/bin/env python3
"""
POSITIVE CONTROLS for W4.6.1 step 1 — Stripe subscriptions. tab-q4vn.

⚠ WHY THIS FILE MATTERS MORE THAN USUAL. `subscriptions_integration_test.go` was written
AFTER the implementation, not before it — red-first was NOT followed for this item, and
saying so plainly is better than implying otherwise. This harness is what stands in for
it: one mutation per behaviour, each required to turn a NAMED test RED on its own. That
is the evidence red-first produces, obtained afterwards, and without it every test here
would be a test that has never been seen to fail.

⚠ C1 IS THE ONE THE ITEM NAMES. W4.6.1: failed payment and cancellation are "where every
naive implementation breaks". C1 removes the out-of-order guard — the single comparison
that stops an older `past_due` from overwriting a newer `active` — and demands the stale
-event test go red. A green C1 means the guard is decoration and the product duns
customers who have already paid.

Convention as w11-display-sweep-controls.py: anchor count asserted before any write, bytes
verified changed, a MUST-RED target AND a MUST-STAY-GREEN companion (both red is SUSPECT,
not CAUGHT), byte-identical sha256-verified restore.

Requires the real-PG harness, same as CI:
    LENS_TEST_DATABASE_URL=postgres://... python3 scripts/w461-subscription-controls-q4vn.py
"""

import hashlib
import os
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
SUBS = REPO / "internal/billing/subscriptions.go"

T_STALE = "TestSubscription_StaleEventDoesNotOverwriteNewerState"
T_CREATED = "TestSubscription_Created_MarksWorkspaceSubscribed"
T_DUP = "TestSubscription_SameEventTwice_OneEventRow"
T_DELETED = "TestSubscription_Deleted_IsTerminal_AndNotSubscribed"
T_SECOND = "TestSubscription_SecondLiveSubscription_NotApplied"
T_FAILED = "TestSubscription_InvoicePaymentFailed_RecordedStatusUnmoved"
T_PASTDUE = "TestSubscription_PastDueIsSubscribed_UnpaidIsNot"
T_ALREADY = "TestSubscriptionCheckout_AlreadySubscribed_RefusesBeforeStripe"


def sha(p: Path) -> str:
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(test: str) -> bool:
    r = subprocess.run(["go", "test", "./internal/billing/", "-run", "^" + test + "$", "-count=1"],
                       cwd=REPO, capture_output=True, text=True)
    if "no test files" in r.stdout or "SKIP" in r.stdout:
        print(f"      ⚠ {test} SKIPPED — LENS_TEST_DATABASE_URL is not set; this harness proves nothing")
        sys.exit(2)
    return r.returncode == 0


def control(cid, desc, old, new, must_red, must_green, expect_count=1):
    src = SUBS.read_text(encoding="utf-8")
    n = src.count(old)
    if n != expect_count:
        print(f"  {cid}  ✗ ANCHOR DEAD — subscriptions.go holds it {n}×, expected {expect_count}")
        return False
    before = sha(SUBS)
    SUBS.write_text(src.replace(old, new, expect_count), encoding="utf-8")
    if sha(SUBS) == before:
        print(f"  {cid}  ✗ THE WRITE CHANGED NOTHING")
        return False
    try:
        red = not run(must_red)
        green = run(must_green)
    finally:
        SUBS.write_text(src, encoding="utf-8")
        assert sha(SUBS) == before, f"{cid}: RESTORE FAILED"
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
        print("A harness whose targets skip reports 5/5 CAUGHT over nothing. Refusing to run.")
        return 2
    print("W4.6.1 step 1 — SUBSCRIPTION POSITIVE CONTROLS (tab-q4vn)\n")
    r = []

    r.append(control(
        "C1", "⚠ the out-of-order guard is removed — an OLDER past_due may overwrite a NEWER "
              "active, i.e. the product duns a customer who has already paid",
        "	stale := haveRow && (!eventAt.After(lastEvent) || terminalStatuses[existing])",
        "	stale := false",
        must_red=T_STALE, must_green=T_CREATED,
    ))

    r.append(control(
        "C2", "a terminal (cancelled) subscription becomes reopenable — a customer is billed "
              "again for a product they cancelled",
        "terminalStatuses[existing])",
        "false)",
        must_red=T_DELETED, must_green=T_CREATED,
    ))

    # ⚠ C3 WAS ORIGINALLY POINTED AT THE ROLLBACK ON THE ALREADY-RECORDED PATH and came back
    # NOT CAUGHT — correctly. A redelivery carries the SAME event.created, so the staleness
    # guard has already set applied=false and the rollback has nothing to undo; the mutation
    # was harmless by construction and the code comment claiming otherwise has been fixed.
    # This is what the idempotency test actually rests on: the UNIQUE + ON CONFLICT that stops
    # a second row existing at all.
    r.append(control(
        "C3", "the event-row idempotency clause is dropped, so a Stripe redelivery raises a "
              "unique violation instead of being absorbed — 5xx, and Stripe retries forever",
        "		ON CONFLICT (stripe_event_id) DO NOTHING`,\n		event.ID, sub.ID, wsID, string(event.Type), status, applied, event.Livemode, eventAt)",
        "		`,\n		event.ID, sub.ID, wsID, string(event.Type), status, applied, event.Livemode, eventAt)",
        must_red=T_DUP, must_green=T_CREATED,
    ))

    r.append(control(
        "C4", "⚠ THE SAVEPOINT IS REMOVED — the double-subscribe insert aborts the whole "
              "transaction (25P02), so the event row explaining it is never written and Stripe "
              "retries forever. This is the bug the row-asserting test actually caught.",
        # ⚠ THE MUTATION DROPS THE SAVEPOINT'S ROLLBACK, not the savepoint. Replacing
        # `tx.Begin` with `tx` also commits the OUTER transaction on the happy path, so
        # the companion went red and the control proved nothing — SUSPECT, correctly.
        # Leaving the savepoint ABORTED is the precise reproduction: the outer
        # transaction stays poisoned (25P02) exactly as it did before the fix, and the
        # happy path is untouched.
        "			if isUniqueViolation(err) {\n				_ = sp.Rollback(ctx)",
        "			if isUniqueViolation(err) {",
        must_red=T_SECOND, must_green=T_CREATED,
    ))

    r.append(control(
        "C5", "invoice.payment_failed starts writing the subscription's status itself — a second "
              "author of a state machine Stripe owns",
        '		event.ID, subID, wsID, string(event.Type), "payment_failed",',
        '		event.ID, subID, wsID, string(event.Type), "past_due",',
        must_red=T_FAILED, must_green=T_CREATED,
    ))

    r.append(control(
        "C6", "past_due stops counting as subscribed — service is cut on the first failed card, "
              "while Stripe is still retrying it",
        '	st.Subscribed = status == "trialing" || status == "active" || status == "past_due"',
        '	st.Subscribed = status == "trialing" || status == "active"',
        must_red=T_PASTDUE, must_green=T_CREATED,
    ))

    r.append(control(
        "C7", "the checkout stops refusing a workspace that already pays, so a second Checkout "
              "Session is created and the collision moves to the webhook — after money has moved",
        '	if live != "" {\n		return "", fmt.Errorf("billing: workspace %s already has a live subscription (%s)", workspaceID, live)\n	}',
        '	_ = live',
        must_red=T_ALREADY, must_green=T_CREATED,
    ))

    print()
    caught = sum(1 for x in r if x)
    print(f"{caught}/{len(r)} controls CAUGHT")
    print("subscriptions.go restored and sha256-verified inside each control")
    return 0 if caught == len(r) else 1


if __name__ == "__main__":
    sys.exit(main())
