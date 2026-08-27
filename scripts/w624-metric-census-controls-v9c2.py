#!/usr/bin/env python3
"""w624-metric-census-controls-v9c2.py — positive controls for the Prometheus metric census.

⚠ Q6 IS THE ONE TO READ. The suffix stripper is a "mechanical exclusion", and its first draft
ATE A REAL METRIC: lens_ha_instance_count is a declared gauge whose name genuinely ends in
`_count`, so stripping before checking turned it into an undeclared name and the guard reported a
working alert rule as firing on a metric that does not exist. A mechanical exclusion that eats a
real name does not just miss findings — it MANUFACTURES them.

Rules, as in the earlier campaigns here.

Run from the repo root:  python3 scripts/w624-metric-census-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

GUARD = "internal/metrics/metric_census_test.go"
METRICS = "internal/metrics/metrics.go"
ALERTS = "deploy/observability/prometheus/alerts.yaml"
PROXY = "internal/proxy/proxy.go"
PKG = "./internal/metrics/"

ZERO = "TestEveryRegisteredMetricCanLeaveZero"
ALERTED = "TestEveryAlertedMetricIsDeclared"
TEETH = "TestMetricCensusCanActuallyClassify"


def sha(p):
    with open(p, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def read(p):
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


def write(p, s):
    with open(p, "w", encoding="utf-8") as f:
        f.write(s)


def run_test(name):
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1", "-v"],
                       capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr


CONTROLS = [
    # ⚠ THE HEADLINE DIRECTION: a SECOND structurally-zero metric appears.
    # ⚠ THE FIRST DRAFT PICKED RequestsTotal, WHICH IS OPERATED IN SEVERAL PLACES, so removing
    # one call left it operated and the control MISSED — a fault in the control, not the guard.
    # UpstreamRequestsTotal is operated in exactly one place (54 of the 62 metrics are), which is
    # what makes it a usable subject.
    ("Q1", "a live metric stops being operated — a second permanent zero on /metrics",
     [(METRICS, "\tUpstreamRequestsTotal.WithLabelValues(provider, status).Inc()",
       "\t_ = provider")],
     ZERO, ALERTED, "its only operation removed, it becomes a permanent 0 on /metrics"),

    ("Q2", "the recorded structurally-zero metric is WIRED — the record must fire, not go quiet",
     [(METRICS, "\t\tDistillTokensSavedTotal.Add(float64(n))",
       "\t\tDistillTokensSavedTotal.Add(float64(n))\n\t\tTokensSavedTotal.WithLabelValues(\"x\", \"y\").Add(1)")],
     ZERO, ALERTED,
     "a record claiming a live metric is dead is worse than no record"),

    ("Q3", "registration is treated as an operation — every metric looks live",
     [(GUARD, 'metricOp = `(?:\\.Inc\\(|\\.Add\\(|\\.Set\\(|\\.Observe\\(|\\.Dec\\(|\\.WithLabelValues\\(|\\.With\\(|\\.SetToCurrentTime\\()`',
       'metricOp = `(?:\\.Inc\\(|\\.Add\\(|\\.Set\\(|\\.Observe\\(|\\.Dec\\(|\\.WithLabelValues\\(|\\.With\\(|\\.SetToCurrentTime\\(|,|\\))`')],
     ZERO, ALERTED,
     "MustRegister is exactly how a structurally-zero series reaches /metrics; counting it as "
     "an operation makes the whole guard vacuous"),

    ("Q4", "an alert rule names a metric nothing declares",
     [(ALERTS, "expr: (max_over_time(lens_ha_instance_count[15m]) - lens_ha_instance_count) >= 1",
       "expr: (max_over_time(lens_ha_instances_typo[15m]) - lens_ha_instances_typo) >= 1")],
     ALERTED, ZERO, "a rule on a metric that does not exist never fires"),

    ("Q5", "break the declaration parse — nothing is declared",
     [(GUARD, 'metricDecl = regexp.MustCompile(`(?m)^\\s*([A-Z][A-Za-z0-9_]*)\\s*=\\s*prometheus\\.New(\\w+)\\(`)',
       'metricDecl = regexp.MustCompile(`(?m)^ZZZ([A-Z]*)ZZZ(\\w+)ZZZ`)')],
     ZERO, "want >= 50", "the non-vacuity floor: no declarations means no structurally-zero metrics"),

    # ⚠ THE ONE THAT ALREADY FIRED FOR REAL.
    ("Q6", "the suffix stripper runs BEFORE the exact-name check again",
     [(GUARD, "\t\t\tif names[m[1]] || names[suffixed(m[1])] || seen[m[1]] {",
       "\t\t\tif names[suffixed(m[1])] || seen[m[1]] {")],
     ALERTED, ZERO,
     "lens_ha_instance_count is a declared gauge ending in _count; stripping first manufactures "
     "a finding out of a working alert rule — this is exactly what the first draft did"),

    # ⚠ MUTATED ON THE GUARD SIDE, NOT THE SOURCE SIDE, AND THE FIRST DRAFT GOT THAT WRONG:
    # renaming prometheus.MustRegister in metrics.go leaves an undefined function, the package
    # does not compile, and the harness saw a build error instead of the guard's own floor
    # message. Changing what the guard LOOKS FOR reproduces "the register block cannot be found"
    # on a tree that still builds.
    # ⚠ THE PRE-FILTER IS A MECHANICAL EXCLUSION AND EXCLUSIONS NEED CONTROLS — that is the
    # lesson W6.24 learned twice already (the _count suffix stripper ate a real metric). This
    # one skips any file that never says "metrics." and is outside internal/metrics.
    ("Q8", "the sweep's pre-filter skips everything — operations outside the metrics package vanish",
     [(GUARD, 'if !strings.Contains(src, "metrics.") && !strings.HasPrefix(rel, "internal/metrics/") {',
       'if true {')],
     TEETH, ALERTED,
     "RequestsTotal is operated in internal/proxy; if the pre-filter hides it the census calls a "
     "live metric structurally zero"),

    ("Q7", "the MustRegister block cannot be found — unpublished metrics get counted",
     [(GUARD, 'i := strings.Index(src, "prometheus.MustRegister(")',
       'i := strings.Index(src, "prometheus.MustRegisterZZZ(")')],
     ZERO, "nothing is published",
     "a declared-but-unregistered metric reaches no scrape; if the register parse breaks the "
     "guard must refuse to run rather than report on series nobody can see"),
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

        red_pass, red_out = run_test(must_red)
        expect_msg = must_green.startswith("want ") or must_green.startswith("nothing ")
        green_pass = True if expect_msg else run_test(must_green)[0]

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if expect_msg:
            # ⚠ NO GREEN COMPANION IS POSSIBLE HERE: these mutations break the parse that EVERY
            # test in the package depends on, so a companion always fails and would be reported
            # as a broken build. The property under test is that the guard's NON-VACUITY FLOOR
            # fires rather than the guard reporting a clean census over nothing, and the failure
            # MESSAGE is what proves that. Same shape as W6.21's N6/N7.
            if red_pass:
                print(f"   ✗ MISSED: {must_red} still PASSES with the defect planted")
                missed.append((cid, "guard blind"))
            elif must_green not in red_out:
                print(f"   ✗ WRONG FAILURE: {must_red} failed but not on its non-vacuity floor "
                      f"({must_green!r})")
                missed.append((cid, "wrong failure"))
            else:
                print(f"   ✓ CAUGHT by {must_red}, on its NON-VACUITY FLOOR ({must_green!r})")
                caught.append(cid)
            continue
        if not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect planted")
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
