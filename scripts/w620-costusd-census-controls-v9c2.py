#!/usr/bin/env python3
"""w620-costusd-census-controls-v9c2.py — positive controls for the unpriced-zero caller census
and for the routing-brain floor record.

⚠ W6.20 REPAIRS NOTHING. Everything it merges is a RECORD, and a record that cannot fail is
worth nothing — so these controls are the whole reason the record is worth having. M1/M2 make
the caller set change in each direction; M5/M6/M7 break each of the three routing-floor
consequences.

Rules, as in the earlier campaigns here: anchor count asserted before editing; a companion test
that must stay green; sha256 re-checked after revert.

Run from the repo root:  python3 scripts/w620-costusd-census-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

CENSUS = "internal/alerts/unpriced_zero_caller_census_test.go"
MCP = "internal/mcp/server.go"
BRAIN = "internal/routingbrain/decide.go"
RECOMMEND = "internal/routingbrain/recommend.go"

ALERTSPKG = "./internal/alerts/"
BRAINPKG = "./internal/routingbrain/"

CENSUST = "TestUnpricedZeroCallerCensus"
SPREAD = "TestTheTwoFallbackDirectionsAreNotClose"
PREMISE = "TestBrainCostBasis_PremiseHolds"
FLOOR = "TestUnpricedRecommendationAlwaysClearsTheCostFloor"
TIE = "TestUnpricedCandidateWinsEveryQualityTie"
SAFEZERO = "TestUnpricedSafeModelRefusesEveryPricedRecommendation"


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.read()


def write(path, s):
    with open(path, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(pkg, name):
    r = subprocess.run(["go", "test", pkg, "-run", "^" + name + "$", "-count=1"],
                       capture_output=True, text=True)
    return r.returncode == 0


CONTROLS = [
    # ── the census, in both directions ──
    ("M1", "a NEW caller of the plain alerts.CostUSD appears and is not in the record",
     [(MCP, "\test := alerts.CostUSD(decision.Model, 500, 500)",
       "\test := alerts.CostUSD(decision.Model, 500, 500)\n\t_ = alerts.CostUSD(decision.Model, 1, 1)")],
     ALERTSPKG, CENSUST, ALERTSPKG, SPREAD,
     "a second call in a listed file must still be seen — the census counts SITES, not files"),

    ("M1b", "a new caller appears in a file the record does not list at all",
     [(BRAIN, "package routingbrain",
       "package routingbrain\n\nimport \"github.com/talyvor/lens/internal/alerts\"\n\nvar _ = alerts.CostUSD(\"m\", 1, 1)")],
     ALERTSPKG, CENSUST, ALERTSPKG, SPREAD,
     "the direction that matters: a new surface silently inherits the zero"),

    ("M2", "a listed caller disappears — a fix nobody recorded the direction of",
     [(MCP, "\test := alerts.CostUSD(decision.Model, 500, 500)", "\test := 0.0")],
     ALERTSPKG, CENSUST, ALERTSPKG, SPREAD,
     "a census that still names a repaired site reads as an open finding"),

    ("M3", "break the sweep — the call regex matches nothing",
     [(CENSUS, 'var costUSDCall = regexp.MustCompile(`\\balerts\\.CostUSD\\(`)',
       'var costUSDCall = regexp.MustCompile(`\\bZZZNoSuchCall\\(`)')],
     ALERTSPKG, CENSUST, ALERTSPKG, SPREAD,
     "the non-vacuity floor: a walk that finds nothing reports a clean census"),

    ("M4", "the comment-only exclusion goes stale",
     [(CENSUS, '\t"internal/proxy/budget_gate_estimate.go",\n}',
       '\t"internal/proxy/budget_gate_estimate.go",\n\t"internal/proxy/lxc_gate.go",\n}')],
     ALERTSPKG, CENSUST, ALERTSPKG, SPREAD,
     "the exclusion list must be a measurement too — this is the failure it already caught once"),

    # ── the routing-floor record ──
    ("M5", "the hard floor stops comparing costs at all",
     [(BRAIN, "\tif recCost > safe.Cost {\n\t\treturn false // (i) exceeds the cost cap — pricier than the safe decision\n\t}",
       "\tif false && recCost > safe.Cost {\n\t\treturn false\n\t}")],
     BRAINPKG, SAFEZERO, BRAINPKG, PREMISE,
     "if the cap never fires, 'an unpriced model clears it' is not a finding about pricing"),

    ("M6", "the candidate tie-break stops using cost",
     [(RECOMMEND, "\tif a.cost != b.cost {\n\t\treturn a.cost < b.cost\n\t}",
       "\tif false && a.cost != b.cost {\n\t\treturn a.cost < b.cost\n\t}")],
     BRAINPKG, TIE, BRAINPKG, PREMISE,
     "the pick's preference for free-looking models is a property of better(), not of the prices"),

    ("M7", "the brain's cost basis stops returning zero for an unpriced model",
     [("internal/routingbrain/unpriced_cost_floor_test.go",
       "func brainCostFn(m string) float64 { return alerts.CostUSD(m, 1000, 1000) }",
       "func brainCostFn(m string) float64 {\n\tv, _ := alerts.CostUSDResolved(m, catalog.PurposeCharge, 1000, 0, 0, 1000)\n\treturn v\n}")],
     BRAINPKG, PREMISE, BRAINPKG, SPREAD,
     "the premise test must fire if the basis is repaired — the record would then be describing "
     "a world that no longer exists"),
]


def main():
    caught, missed = [], []
    for cid, desc, edits, pkg, must_red, gpkg, must_green, note in CONTROLS:
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

        red_pass = run_test(pkg, must_red)
        green_pass = run_test(gpkg, must_green)

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
