#!/usr/bin/env python3
"""w616-stream-unpriced-cost-controls-v9c2.py — positive controls for the streamed-settle
unpriced-model fix.

Same three rules as the earlier campaigns here:
  1. anchor count asserted BEFORE editing;
  2. every control names a companion test that must stay GREEN;
  3. sha256 before the edit, re-checked after the revert.

⚠ I2 AND I3 ARE THE ONES TO READ. This change touches a MONEY path — the reservation settle
is the customer's bill — so the controls that matter most are not the ones proving the defect
comes back, but the ones proving the fix does not move a model the catalog actually prices.

Run from the repo root:  python3 scripts/w616-stream-unpriced-cost-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

PROXY = "internal/proxy/proxy.go"
ALERTS = "internal/alerts/alerts.go"
RESOLVE = "internal/catalog/resolve.go"
TEST = "internal/proxy/stream_unpriced_cost_test.go"

PKG = "./internal/proxy/"

UNPRICED = "TestStreamSettle_UnpricedModelStillReachesTheBudget"
UNPRICED_USAGE = "TestStreamSettle_UnpricedModelWithProviderUsageStillReachesTheBudget"
PRICED = "TestStreamSettle_PricedModelIsUnchanged"
CACHEAWARE = "TestStreamSettle_PricedModelWithCacheBreakdownIsByteIdentical"
AGREE = "TestStreamedAndBufferedPriceTheSameRequestTheSameWay"
PREMISE = "TestCatalogStillDoesNotPriceTheProbeModel"

ESTIMATE_LINE = "servedCostUSD, _ = alerts.CostUSDResolved(sc.model, catalog.PurposeCharge, inT, 0, 0, outT)"
USAGE_LINES = """		servedCostUSD, _ = alerts.CostUSDResolved(sc.model, catalog.PurposeCharge,
			u.uncachedInputTokens, u.cachedInputTokens, u.cacheWriteInputTokens, u.outputTokens)"""


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
    ("I1", "revert the ESTIMATE branch — the flat estimate prices through the plain helper again",
     [(PROXY, ESTIMATE_LINE, "servedCostUSD = alerts.CostUSD(sc.model, inT, outT)")],
     UNPRICED, PRICED, "the defect as it shipped, on the branch with no provider usage"),

    ("I1b", "the same revert, seen by the two-paths-agree test",
     [(PROXY, ESTIMATE_LINE, "servedCostUSD = alerts.CostUSD(sc.model, inT, outT)")],
     AGREE, PRICED, "streamed and buffered book different amounts for one request"),

    ("I1c", "revert the PROVIDER-USAGE branch — fixing only one branch is not fixing it",
     [(PROXY, USAGE_LINES,
       "\t\tservedCostUSD = alerts.CostUSDDetailed(sc.model, u.uncachedInputTokens, "
       "u.cachedInputTokens, u.cacheWriteInputTokens, u.outputTokens)")],
     UNPRICED_USAGE, UNPRICED, "CostUSDDetailed is zero for an unpriced model too"),

    # ⚠ THE CONTROLS THAT MATTER MOST ON A MONEY PATH: the fix must not move a priced model.
    ("I2", "the resolver is made to ignore the exact price and always fall back",
     [(RESOLVE, "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); ok && !unpriced(in, out) {",
       "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); false && ok && !unpriced(in, out) {")],
     PRICED, PREMISE,
     "if the fix started repricing KNOWN models, this is the test that says so"),

    ("I3", "the same, seen by the cache-aware byte-identity guard",
     [(RESOLVE, "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); ok && !unpriced(in, out) {",
       "\tif in, cachedIn, cacheWrite, out, ok := r.PriceDetailed(id); false && ok && !unpriced(in, out) {")],
     CACHEAWARE, PREMISE,
     "the provider-usage branch with a cache breakdown must be byte-identical to before"),

    # ⚠ THE PREMISE CONTROL. Every test here rests on gpt-4 being ABSENT from the catalog.
    ("I4", "the probe model is priced after all — the premise must fail loudly, not quietly",
     [(TEST, 'const unpricedProbeModel = "gpt-4"', 'const unpricedProbeModel = "gpt-4o"')],
     PREMISE, PRICED,
     "a test whose premise has gone stale must say so instead of going green"),

    ("I5", "the budget feed is cut — the settle computes a cost and records nothing",
     [(PROXY, "\t\tp.budgetService.RecordSpend(ctx, sc.wsID, sc.team, sc.sprint, servedCostUSD)",
       "\t\t_ = servedCostUSD")],
     UNPRICED, PREMISE,
     "the non-vacuity check must fire: a cost nobody records is not a budget feed"),
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

        red_pass = run_test(must_red)
        green_pass = run_test(must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

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
