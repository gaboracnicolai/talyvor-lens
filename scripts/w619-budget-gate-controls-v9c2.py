#!/usr/bin/env python3
"""w619-budget-gate-controls-v9c2.py — positive controls for the budget gate's estimate.

⚠ L2 AND L3 ARE THE POINT. This touches a GATE on live traffic, so the controls that matter are
not the ones proving the defect returns — they are the ones proving the fix does not start
refusing requests it used to allow. A priced model must estimate exactly what it estimated
before, and the fallback must be the CHARGE one (cheapest), not the HOLD one: for a 10k-token
request on gpt-4 those differ by 1500x ($0.0002 vs $0.30), and picking Hold would turn an
under-blocking gate into an over-blocking one.

Rules, as in the earlier campaigns here: anchor count asserted before editing; a companion test
that must stay green; sha256 re-checked after revert.

Run from the repo root:  python3 scripts/w619-budget-gate-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

EST = "internal/proxy/budget_gate_estimate.go"
PROXY = "internal/proxy/proxy.go"
LXC = "internal/proxy/lxc_gate.go"

PKG = "./internal/proxy/"

NOTZERO = "TestBudgetEstimate_UnpricedModelIsNotZero"
MIRRORS = "TestBudgetEstimate_MatchesTheLXCGatesBasis"
PRICED = "TestBudgetEstimate_PricedModelIsUnchangedFromTheOldHelper"
PURPOSE = "TestBudgetEstimate_UsesTheChargeFallbackNotTheHoldFallback"
EMPTY = "TestBudgetEstimate_EmptyPromptIsStillZero"
WIRED = "TestBudgetGateUsesTheNamedEstimate"

RESOLVED = "estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)"


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
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1"],
                       capture_output=True, text=True)
    return r.returncode == 0


CONTROLS = [
    ("L1", "revert the fix — the gate prices through the plain helper again",
     [(EST, RESOLVED, "estUSD, prov := alerts.CostUSD(model, len(prompt)/4, 0), catalog.ProvenanceExact")],
     NOTZERO, PRICED, "the defect exactly as it shipped: zero for any unpriced model"),

    ("L1b", "the same revert, seen by the two-gates-must-agree test",
     [(EST, RESOLVED, "estUSD, prov := alerts.CostUSD(model, len(prompt)/4, 0), catalog.ProvenanceExact")],
     MIRRORS, PRICED, "the budget gate and the LXC gate price one request two ways"),

    # ⚠ THE DIRECTION CONTROL. Hold falls back to the provider's most expensive known model:
    # 1500x the charge fallback on this probe. A gate that used it would refuse traffic it
    # currently allows.
    ("L2", "the gate switches to the HOLD fallback — an under-blocking gate becomes over-blocking",
     [(EST, "catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)",
       "catalog.PurposeHold, len(prompt)/4, 0, 0, 0)")],
     PURPOSE, NOTZERO, "picking the purpose by taste is how a gate starts refusing paying traffic"),

    ("L3", "the resolver is made to ignore exact prices and always fall back",
     [("internal/catalog/resolve.go",
       "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); ok && !unpriced(in, out) {",
       "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); false && ok && !unpriced(in, out) {")],
     PRICED, EMPTY,
     "if the change started re-estimating the 45 models the catalog HOLDS, this is what says so"),

    ("L4", "the empty-prompt case starts producing an estimate",
     [(EST, "\tif prov == catalog.ProvenanceFallback && len(prompt) > 0 {",
       "\tif prov == catalog.ProvenanceFallback && len(prompt) >= 0 {")],
     None, EMPTY,
     "the warning suppression on an empty prompt must not change the VALUE — a must-not-move check"),

    ("L5", "the gate stops calling the named estimate",
     [(PROXY, "estCost := budgetEstimateUSD(model, prompt)",
       "estCost := alerts.CostUSD(model, len(prompt)/4, 0)")],
     WIRED, NOTZERO, "the function can be perfect and unreached"),

    # ⚠ THE PRECEDENT THIS ITEM RESTS ON. lxcEstimate is the pre-serve GATE that already uses
    # PurposeCharge; if it moves, the "mirror the LXC gate" argument is stale and the mirror
    # test must say so rather than quietly agree with a new answer.
    ("L6", "the LXC gate's own basis changes — the mirror argument goes stale",
     [(LXC, "estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeCharge, len(prompt)/4, 0, 0, 0)",
       "estUSD, prov := alerts.CostUSDResolved(model, catalog.PurposeHold, len(prompt)/4, 0, 0, 0)")],
     MIRRORS, NOTZERO, "this item mirrors lxcEstimate; if lxcEstimate moves, the record is stale"),
]


def main():
    caught, missed = [], []
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

        red_pass = run_test(must_red) if must_red else None
        green_pass = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if must_red is None:
            if green_pass:
                print(f"   ✓ CORRECT SILENCE: {must_green} stayed green, as it must")
                caught.append(cid)
            else:
                print(f"   ✗ FALSE POSITIVE / VALUE MOVED: {must_green} fired")
                missed.append((cid, "value moved"))
            continue

        if not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect reintroduced")
            missed.append((cid, "guard blind"))
        else:
            print(f"   ✓ CAUGHT by {must_red} (companion {must_green} stayed green)")
            caught.append(cid)

    print()
    print(f"CAUGHT {len(caught)}: {', '.join(caught) or '—'}")
    print(f"MISSED {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
