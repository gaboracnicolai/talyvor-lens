#!/usr/bin/env python3
"""w625-wiring-seam-controls-v9c2.py — positive controls for the wiring-seam census.

⚠ R4 AND R5 ARE THE ONES TO READ. This census lied TWICE before it was right, and both lies
pointed the same way — calling a LIVE seam dead. R4 removes the same-package caller search
(which wrongly flagged localrouter.SetNodePrice and benchprobe.SetProbeScore) and R5 removes the
bench_*.go exclusion (which wrongly flagged proxy.SetAnthropicURL / SetGoogleURL). A census whose
errors all flatter the reporter is the one to distrust.

Rules, as in the earlier campaigns here.

Run from the repo root:  python3 scripts/w625-wiring-seam-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

GUARD = "internal/seams/wiring_seam_census_test.go"
MAIN = "cmd/lens/main.go"
PKG = "./internal/seams/"

SEAMS = "TestEveryWiringSeamHasAProductionCaller"
TEETH = "TestSeamCensusCanActuallyClassify"


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
    # ⚠ THE DIRECTION THAT MATTERS: somebody acts on the record.
    ("R1", "a recorded unwired seam is WIRED — the record must fire, not go quiet",
     [(MAIN, "\tbatchGate := newBatchReg(cfg.BatchEnabled, false)",
       "\tbatchRouter.SetSettleHook(nil)\n\tbatchGate := newBatchReg(cfg.BatchEnabled, false)")],
     SEAMS, TEETH,
     "the record says things about the system (why the batch lane cannot open) that stop being "
     "true the moment somebody plugs the socket in"),

    ("R2", "a NEW seam appears with no production caller and nobody recorded it",
     [("internal/batch/router.go", "func (r *BatchRouter) SetSettleHook(",
       "func (r *BatchRouter) SetZzzUnrecordedHook(fn func()) {}\n\nfunc (r *BatchRouter) SetSettleHook(")],
     SEAMS, TEETH, "an unplugged socket that reads as finished is the whole class"),

    ("R3", "break the seam declaration parse",
     [(GUARD, '(?:Set|Register|Enable|Attach|Wire)[A-Z]', '(?:ZzzNoSuchPrefix)[A-Z]')],
     # ⚠ ALSO MESSAGE-ASSERTED: zero seams parsed fails EVERY test in the package, so no green
     # companion exists. What must be true is that the census stops on its NON-VACUITY FLOOR
     # rather than reporting an empty unwired set as a clean result.
     SEAMS, "the declaration parse has broken",
     "the non-vacuity floor: no seams parsed means no unwired seams"),

    # ⚠ THE TWO LIES THE CENSUS ACTUALLY TOLD, MADE INTO CONTROLS.
    ("R4", "the caller sweep skips the declaring package again",
     [(GUARD, "\t\t\tif pats[s.method].MatchString(src) {\n\t\t\t\thit[s.key()] = rel\n\t\t\t}",
       "\t\t\tif pats[s.method].MatchString(src) && !strings.HasPrefix(rel, s.pkgDir) {\n\t\t\t\thit[s.key()] = rel\n\t\t\t}")],
     SEAMS, TEETH,
     "localrouter.SetNodePrice is called by its own package's price-sync loop and "
     "benchprobe.SetProbeScore by its own scheduler; the first draft called both dead"),

    ("R5", "the bench_*.go exclusion is removed — test seams look like unplugged sockets",
     [(GUARD, '\t\tif strings.Contains(filepath.Base(path), "bench_") {\n\t\t\treturn nil\n\t\t}',
       '\t\tif false {\n\t\t\treturn nil\n\t\t}')],
     SEAMS, TEETH,
     "proxy.SetAnthropicURL/SetGoogleURL live in bench_setters.go and carry no doc comment; the "
     "file name is the evidence a keyword scan missed"),

    ("R6", "the caller sweep matches everything — every seam looks wired",
     [(GUARD, '\t\t\tpats[s.method] = regexp.MustCompile(`\\.` + s.method + `\\(`)',
       '\t\t\tpats[s.method] = regexp.MustCompile(`.`)')],
     # ⚠ NO GREEN COMPANION IS POSSIBLE: a sweep that matches everything empties the unwired
     # set, so the census ALSO fails (on its "recorded but now wired" branch). The property is
     # that the teeth test notices the sweep saying yes to everything, and the message proves it.
     TEETH, "the sweep counts a test as production",
     "\"the unwired set is exactly this\" is satisfied by an empty set, so the teeth test is the "
     "only thing that notices a sweep which says yes to everything"),
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
        expect_msg = must_green.startswith("the sweep ")
        green_pass = True if expect_msg else run_test(must_green)[0]

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if expect_msg:
            if red_pass:
                print(f"   ✗ MISSED: {must_red} still PASSES with the defect planted")
                missed.append((cid, "guard blind"))
            elif must_green not in red_out:
                print(f"   ✗ WRONG FAILURE: {must_red} failed, but not on {must_green!r}")
                missed.append((cid, "wrong failure"))
            else:
                print(f"   ✓ CAUGHT by {must_red}, on the expected message ({must_green!r})")
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
