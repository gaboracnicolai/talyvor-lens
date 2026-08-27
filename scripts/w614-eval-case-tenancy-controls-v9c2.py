#!/usr/bin/env python3
"""w614-eval-case-tenancy-controls-v9c2.py — positive controls for the POST /v1/eval/cases
cross-tenant create fix and for the executed record of what a planted case does.

Same three rules as the earlier campaigns in this directory:
  1. anchor count asserted BEFORE editing;
  2. every control names a companion test that must stay GREEN, so a broken build cannot
     masquerade as a catch;
  3. sha256 before the edit, re-checked after the revert.

G5/G6 drive real-Postgres tests; without LENS_TEST_DATABASE_URL they SKIP and are reported
UNARMED rather than counted.

Run from the repo root:  python3 scripts/w614-eval-case-tenancy-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

HANDLER = "cmd/lens/eval_case_create_handler.go"
MAIN = "cmd/lens/main.go"
PIPELINE = "internal/eval/pipeline.go"

LENSPKG = "./cmd/lens/"
EVALPKG = "./internal/eval/"

CROSS = "TestEvalCaseCreate_NonAdminCannotNameAnotherWorkspace"
ADMIN = "TestEvalCaseCreate_AdminStillHonoursTheBody"
HONEST = "TestEvalCaseCreate_HonestCallerIsUnaffected"
NOIDENT = "TestEvalCaseCreate_NoWorkspaceIdentityIsRefused"
ROW = "TestEvalCaseCreate_RowLandsInTheCallersWorkspace_RealPG"
WIRED = "TestEvalCaseCreateRouteGoesThroughTheScopedHandler"
REACHES = "TestEvalCaseCreateReachesAnIdentityDecision"

EXECUTES = "TestRunSuite_ExecutesEveryStoredCaseWithItsOwnProviderModelAndPrompt"
NOTOTHERS = "TestRunSuite_DoesNotRunAnotherWorkspacesCases"

SCOPING = """		eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		in.WorkspaceID = eff
"""


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
    r = subprocess.run(["go", "test", pkg, "-run", "^" + name + "$", "-count=1", "-v"],
                       capture_output=True, text=True)
    return r.returncode == 0, ("--- SKIP: " + name) in r.stdout


# (id, description, edits, pkg, must_red, gpkg, must_stay_green, note)
CONTROLS = [
    ("G1", "revert the fix — the create takes its workspace from the request body again",
     [(HANDLER, SCOPING, "")],
     LENSPKG, CROSS, LENSPKG, ADMIN, "the defect exactly as it shipped"),

    ("G1b", "the same revert, seen by the persisted ROW on real Postgres",
     [(HANDLER, SCOPING, "")],
     LENSPKG, ROW, LENSPKG, ADMIN, "the case lands in ws-victim's suite"),

    ("G1c", "the same revert, seen by the residue guard",
     [(HANDLER, SCOPING, "")],
     LENSPKG, REACHES, LENSPKG, ADMIN,
     "the route falls back into the W6.13 residue: authed, no {wsID}, no identity decision"),

    ("G2", "the fix honours the body for everyone, not just the admin",
     [(HANDLER, "eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)",
       "eff, _, ok := in.WorkspaceID, false, true")],
     LENSPKG, CROSS, LENSPKG, HONEST, "a narrowing that is not a narrowing"),

    ("G3", "the fix forces EVERYONE to their own workspace, taking the admin's seed with it",
     [(HANDLER, "eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)",
       "eff, _, ok := effectiveWorkspaceID(req, \"\")")],
     LENSPKG, ADMIN, LENSPKG, CROSS, "the opposite error, seen only by the mirror test"),

    ("G4", "the 403 becomes a fall-through to the body when no identity resolves",
     [(HANDLER, "\t\tif !ok {\n\t\t\twriteJSONErr(w, http.StatusForbidden, \"forbidden: no workspace identity\")\n\t\t\treturn\n\t\t}\n\t\tin.WorkspaceID = eff",
       "\t\tif ok {\n\t\t\tin.WorkspaceID = eff\n\t\t}")],
     LENSPKG, NOIDENT, LENSPKG, CROSS, "a shared bucket is how tenants meet each other"),

    # ⚠ THE CONSEQUENCE CONTROLS. The create-side tests above are only worth their severity
    # because a stored case is EXECUTED with its own provider/model/prompt. These prove that
    # half can fail.
    ("G5", "RunSuite stops passing the case's own model to the provider",
     [(PIPELINE, "response, err := p.callLLM(callCtx, tc.Provider, tc.Model, tc.Prompt)",
       "response, err := p.callLLM(callCtx, tc.Provider, \"pinned-model\", tc.Prompt)")],
     EVALPKG, EXECUTES, EVALPKG, NOTOTHERS,
     "if the model were pinned, 'the attacker chooses what you pay for' would be false"),

    # ⚠ THE ANCHOR HERE IS THE SQL, NOT THE CALL SITE, AND THE FIRST DRAFT GOT THAT WRONG:
    # passing "" to ListTestCases makes it substitute "default", so ws-victim's own case
    # stopped running too and BOTH tests went red — which the companion rule reported as
    # "companion red" rather than letting it count as a catch. Mutating the WHERE clause
    # widens the population without moving the victim's own case out of it.
    ("G6", "the workspace filter on the suite stops binding — every tenant's cases run",
     [(PIPELINE, "FROM eval_test_cases WHERE workspace_id = $1`",
       "FROM eval_test_cases WHERE $1 = $1`")],
     EVALPKG, NOTOTHERS, EVALPKG, EXECUTES,
     "the scoping half of the mechanism, which is what BOUNDS this finding"),

    ("G7", "the route stops going through the scoped handler",
     [(MAIN, 'authed.Post("/v1/eval/cases", newEvalCaseCreateHandler(evalPipeline))',
       'authed.Post("/v1/eval/cases", func(w http.ResponseWriter, req *http.Request) {\n'
       '\t\t\tvar in eval.TestCase\n'
       '\t\t\tif err := json.NewDecoder(req.Body).Decode(&in); err != nil {\n'
       '\t\t\t\twriteJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())\n'
       '\t\t\t\treturn\n'
       '\t\t\t}\n'
       '\t\t\tcreated, err := evalPipeline.AddTestCase(req.Context(), in)\n'
       '\t\t\tif err != nil {\n'
       '\t\t\t\twriteJSONErr(w, http.StatusBadRequest, err.Error())\n'
       '\t\t\t\treturn\n'
       '\t\t\t}\n'
       '\t\t\twriteJSONOK(w, http.StatusCreated, created)\n'
       '\t\t})')],
     LENSPKG, WIRED, LENSPKG, CROSS,
     "the handler can be perfect and unreached — every other test drives it directly"),
]


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("⚠ LENS_TEST_DATABASE_URL is unset — G1b/G5/G6 drive real-PG tests that will SKIP.\n")

    caught, missed, unarmed = [], [], []
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
                print(f"     anchor: {old[:100]!r}")
                ok = False
                break
            write(path, cur.replace(old, new, 1))
        if not ok:
            for path, s in originals.items():
                write(path, s)
            missed.append((cid, "anchor did not apply"))
            continue

        red_pass, red_skip = run_test(pkg, must_red)
        green_pass, green_skip = run_test(gpkg, must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if red_skip:
            print(f"   ⚠ UNARMED: {must_red} SKIPPED (needs a database)")
            unarmed.append((cid, must_red))
            continue
        if not green_skip and not green_pass:
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
    print(f"CAUGHT  {len(caught)}: {', '.join(caught) or '—'}")
    print(f"UNARMED {len(unarmed)}: {', '.join(c for c, _ in unarmed) or '—'}")
    print(f"MISSED  {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
