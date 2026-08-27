#!/usr/bin/env python3
"""w617-batch-submit-tenancy-controls-v9c2.py — positive controls for the
POST /v1/batch/submit workspace-derivation fix and for the lane/job-list coupling guard.

Same three rules as the earlier campaigns here:
  1. anchor count asserted BEFORE editing;
  2. every control names a companion test that must stay GREEN;
  3. sha256 before the edit, re-checked after the revert.

⚠ J6 IS THE ONE TO READ. The coupling guard is CONDITIONAL — it enforces nothing while the
lane is closed, which is a guard that cannot fail in today's tree. J6 opens the lane in main.go
and requires it to fire. Without J6 that test is decoration.

Run from the repo root:  python3 scripts/w617-batch-submit-tenancy-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

HANDLER = "cmd/lens/batch_submit_handler.go"
MAIN = "cmd/lens/main.go"
ROUTER = "internal/batch/router.go"

PKG = "./cmd/lens/"

CROSS = "TestBatchSubmit_HeaderCannotNameAnotherWorkspace"
NOHEADER = "TestBatchSubmit_MissingHeaderDoesNotLandInASharedDefaultBucket"
ADMIN = "TestBatchSubmit_AdminStillHonoursTheHeader"
ADMINDEF = "TestBatchSubmit_AdminWithNoHeaderKeepsTheDefaultBucket"
NOIDENT = "TestBatchSubmit_NoWorkspaceIdentityIsRefused"
ELIG = "TestBatchSubmit_IneligibleBodyIsStillRefused"
WIRED = "TestBatchSubmitRouteGoesThroughTheScopedHandler"
COUPLED = "TestBatchLane_CannotOpenWhileTheJobListIsUnscoped"

SCOPING = '''		wsID, _, ok := effectiveWorkspaceID(req, req.Header.Get("X-Talyvor-Workspace"))
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		if wsID == "" {
			wsID = "default"
		}
'''
HEADER_ONLY = '''		wsID := req.Header.Get("X-Talyvor-Workspace")
		if wsID == "" {
			wsID = "default"
		}
'''


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
    ("J1", "revert the fix — the workspace comes from the header again",
     [(HANDLER, SCOPING, HEADER_ONLY)],
     CROSS, ADMIN, "the defect exactly as it sat behind the closed door"),

    ("J1b", "the same revert, seen by the unheadered case",
     [(HANDLER, SCOPING, HEADER_ONLY)],
     NOHEADER, ADMIN, "every unheadered tenant lands in one shared \"default\" bucket"),

    ("J1c", "the same revert, seen by the no-identity case",
     [(HANDLER, SCOPING, HEADER_ONLY)],
     NOIDENT, ADMIN, "a caller with no workspace submits anyway"),

    ("J2", "the fix honours the header for everyone, not just the admin",
     [(HANDLER, 'wsID, _, ok := effectiveWorkspaceID(req, req.Header.Get("X-Talyvor-Workspace"))',
       'wsID, _, ok := req.Header.Get("X-Talyvor-Workspace"), false, true')],
     CROSS, ADMINDEF, "a narrowing that is not a narrowing"),

    ("J3", "the fix forces EVERYONE to their own workspace, taking the operator's reach with it",
     [(HANDLER, 'wsID, _, ok := effectiveWorkspaceID(req, req.Header.Get("X-Talyvor-Workspace"))',
       'wsID, _, ok := effectiveWorkspaceID(req, "")')],
     ADMIN, CROSS, "the opposite error, seen only by the mirror test"),

    ("J4", "the eligibility gate is skipped once the workspace is resolved",
     [(HANDLER, "\t\tif !elig.Eligible {", "\t\tif false && !elig.Eligible {")],
     ELIG, CROSS, "narrowing authz must not disturb the gate that was already there"),

    ("J5", "the route stops going through the scoped handler",
     [(MAIN, 'batchGate.post(authed, "/v1/batch/submit", newBatchSubmitHandler(batchRouter))',
       'batchGate.post(authed, "/v1/batch/submit", func(w http.ResponseWriter, req *http.Request) {\n'
       '\t\t\twsID := req.Header.Get("X-Talyvor-Workspace")\n'
       '\t\t\t_ = wsID\n'
       '\t\t\twriteJSONOK(w, http.StatusAccepted, map[string]any{})\n'
       '\t\t})')],
     WIRED, CROSS, "the handler can be perfect and unreached"),

    # ⚠ THE ONE THAT MATTERS FOR THE COUPLING GUARD. It enforces NOTHING while the lane is
    # closed — which is exactly the "guard that cannot fail" shape this queue keeps catching.
    # Opening the lane must make it fire, or it is decoration.
    ("J6", "the lane is OPENED while BatchRouter.ListJobs still takes no workspace",
     [(MAIN, "batchGate := newBatchReg(cfg.BatchEnabled, false)",
       "batchGate := newBatchReg(cfg.BatchEnabled, true)")],
     COUPLED, CROSS,
     "an open lane hands every tenant every other tenant's job, Prompt included"),

    # ⚠ THE CALL SITE IS EDITED TOO, AND ITS FIRST DRAFT DID NOT DO THAT. Renaming ListJobs
    # without fixing main.go's caller breaks the BUILD, the test fails, and a "must not fire"
    # control reports that as the guard firing — a false positive about a false positive. The
    # companion-green rule catches this shape everywhere else; here the mutation has to leave a
    # COMPILING tree or it measures nothing.
    ("J6b", "the lane is opened AND ListJobs is scoped — the guard must NOT fire",
     [(MAIN, "batchGate := newBatchReg(cfg.BatchEnabled, false)",
       "batchGate := newBatchReg(cfg.BatchEnabled, true)"),
      (MAIN, "batchRouter.ListJobs()", 'batchRouter.ListJobs("")'),
      (ROUTER, "func (r *BatchRouter) ListJobs() []*BatchJob {",
       "func (r *BatchRouter) ListJobs(workspaceID string) []*BatchJob {"),
      (ROUTER, "\tout := make([]*BatchJob, 0, len(r.pending))",
       "\t_ = workspaceID\n\tout := make([]*BatchJob, 0, len(r.pending))")],
     None, COUPLED,
     "the mirror: a guard that fires on an OPEN lane with a SCOPED list would block the fix"),
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
            # A "must NOT fire" control: only the companion matters.
            if green_pass:
                print(f"   ✓ CORRECT SILENCE: {must_green} stayed green, as it must")
                caught.append(cid)
            else:
                print(f"   ✗ FALSE POSITIVE: {must_green} fired when it should not have")
                missed.append((cid, "false positive"))
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
