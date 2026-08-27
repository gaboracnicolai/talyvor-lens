#!/usr/bin/env python3
"""w613-prompt-create-tenancy-controls-v9c2.py — positive controls for the POST /v1/prompts
cross-tenant create fix.

Same three rules as the earlier campaigns in this directory:
  1. anchor count asserted BEFORE editing — a substitution that matches nothing edits zero bytes,
     and "the control did not apply" is byte-indistinguishable from "the guard caught it";
  2. every control names a companion test that must stay GREEN, so a mutation that breaks the
     build cannot masquerade as a catch;
  3. sha256 taken before the edit and re-checked after the revert.

F5/F6 drive the real-Postgres test; without LENS_TEST_DATABASE_URL it SKIPs and is reported
UNARMED rather than counted.

Run from the repo root:  python3 scripts/w613-prompt-create-tenancy-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

HANDLER = "cmd/lens/prompt_create_handler.go"
TEST = "cmd/lens/prompt_create_handler_test.go"
MAIN = "cmd/lens/main.go"
MGR = "internal/prompts/manager.go"

PKG = "./cmd/lens/"

CROSS = "TestPromptCreate_NonAdminCannotNameAnotherWorkspace"
ADMIN = "TestPromptCreate_AdminStillHonoursTheBody"
HONEST = "TestPromptCreate_HonestCallerNamingItsOwnWorkspaceIsUnaffected"
NOIDENT = "TestPromptCreate_NoWorkspaceIdentityIsRefused"
RESOLVE = "TestPromptCreate_DoesNotChangeWhatAnotherTenantsRequestResolvesTo"
ROW = "TestPromptCreate_RowLandsInTheCallersWorkspace_RealPG"
WIRED = "TestPromptCreateRouteGoesThroughTheScopedHandler"


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
    r = subprocess.run(["go", "test", PKG, "-run", "^" + name + "$", "-count=1", "-v"],
                       capture_output=True, text=True)
    return r.returncode == 0, ("--- SKIP: " + name) in r.stdout


SCOPING = """		eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "forbidden: no workspace identity")
			return
		}
		in.WorkspaceID = eff
"""

# (id, description, edits, must_red, must_stay_green, note)
CONTROLS = [
    ("F1", "revert the fix — the create takes its workspace from the request body again",
     [(HANDLER, SCOPING, "")],
     CROSS, ADMIN, "the defect exactly as it shipped"),

    # ⚠ THE ONE THAT MATTERS. The handler-seam test above could be satisfied by any
    # narrowing; this proves the CONSEQUENCE is what moved — the victim's own outbound
    # request, through the same Resolve call proxy.go makes on every request.
    ("F1b", "the same revert, seen by the SERVING-PATH test rather than by the handler seam",
     [(HANDLER, SCOPING, "")],
     RESOLVE, ADMIN, "ws-victim's resolved body carries ATTACKER-TEXT again"),

    ("F1c", "the same revert, seen by the persisted ROW on real Postgres",
     [(HANDLER, SCOPING, "")],
     ROW, ADMIN, "the row lands in ws-victim"),

    ("F2", "the fix honours the body for everyone, not just the admin",
     [(HANDLER, "eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)",
       "eff, _, ok := in.WorkspaceID, false, true")],
     CROSS, HONEST, "a narrowing that is not a narrowing"),

    ("F3", "the fix forces EVERYONE to their own workspace, taking the admin's reach with it",
     [(HANDLER, "eff, _, ok := effectiveWorkspaceID(req, in.WorkspaceID)",
       "eff, _, ok := effectiveWorkspaceID(req, \"\")")],
     ADMIN, CROSS, "the opposite error, and the mirror test is the only thing that sees it"),

    ("F4", "the 403 becomes a fall-through to \"default\" when no identity resolves",
     [(HANDLER, "\t\tif !ok {\n\t\t\twriteJSONErr(w, http.StatusForbidden, \"forbidden: no workspace identity\")\n\t\t\treturn\n\t\t}\n\t\tin.WorkspaceID = eff",
       "\t\tif !ok {\n\t\t\teff = \"default\"\n\t\t}\n\t\tin.WorkspaceID = eff")],
     NOIDENT, CROSS, "a shared/\"default\" bucket is how tenants meet each other"),

    # ⚠ VACUITY CONTROL on the serving-path test: if the victim's prompt never resolved
    # in the first place, "the attacker did not replace it" is true and worthless.
    ("F5", "blind the serving-path test — Resolve stops substituting anything",
     [(MGR, 'if !bytes.Contains(body, []byte("lens:prompt:")) {',
       'if true || !bytes.Contains(body, []byte("lens:prompt:")) {')],
     RESOLVE, CROSS, "the ARMED check must fire, not the attack assertion"),

    ("F6", "the route stops going through the scoped handler",
     [(MAIN, 'authed.Post("/v1/prompts", newPromptCreateHandler(promptManager))',
       'authed.Post("/v1/prompts", func(w http.ResponseWriter, req *http.Request) {\n'
       '\t\t\tvar in prompts.Prompt\n'
       '\t\t\tif err := json.NewDecoder(req.Body).Decode(&in); err != nil {\n'
       '\t\t\t\twriteJSONErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())\n'
       '\t\t\t\treturn\n'
       '\t\t\t}\n'
       '\t\t\tcreated, err := promptManager.Create(req.Context(), in)\n'
       '\t\t\tif err != nil {\n'
       '\t\t\t\twriteJSONErr(w, http.StatusBadRequest, err.Error())\n'
       '\t\t\t\treturn\n'
       '\t\t\t}\n'
       '\t\t\twriteJSONOK(w, http.StatusCreated, created)\n'
       '\t\t})')],
     WIRED, CROSS,
     "the handler can be perfect and unreached — the tests above drive it directly"),
]


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("⚠ LENS_TEST_DATABASE_URL is unset — F1c drives a real-PG test that will SKIP.\n")

    caught, missed, unarmed = [], [], []
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
                print(f"     anchor: {old[:100]!r}")
                ok = False
                break
            write(path, cur.replace(old, new, 1))
        if not ok:
            for path, s in originals.items():
                write(path, s)
            missed.append((cid, "anchor did not apply"))
            continue

        red_pass, red_skip = run_test(must_red)
        green_pass, green_skip = run_test(must_green)

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
