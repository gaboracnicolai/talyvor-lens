#!/usr/bin/env python3
"""w612-admin-gate-presence-controls-v9c2.py — the control campaign for the /v1/admin gate-presence
guard, and the DEMONSTRATION that the existing classification guard is blind to a missing gate.

TestEveryAdminRegistrationReachesAnAuthorizationDecision passes on its first run — every /v1/admin
route is gated at main today — so it is worth nothing until something shows it can fail. That is
what this is for, and it does double duty: for E1, E2, E3 and E5 the "must stay green" companion is
admin_route_classification_test.go's OWN test, so each line of output is simultaneously
  (a) proof the mutation compiled, and
  (b) proof the existing guard went GREEN over a money route whose gate had just been deleted.

E4 is the contrast case, kept deliberately: the existing guard is asymmetric, not useless — when an
operatorReadable route loses its wrapper it DOES catch it. The asymmetry is the finding.

Rules, as in scripts/w64-credential-leak-controls.py:
  1. anchor count asserted before editing  2. a named companion that must stay green
  3. sha256 re-checked after the revert

Run from the repo root:  python3 scripts/w612-admin-gate-presence-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

MAIN = "cmd/lens/main.go"
ADJ = "cmd/lens/adjudication_handler.go"
GATE = "cmd/lens/admin_route_gate_presence_test.go"
SESSION = "cmd/lens/session_key_handler.go"

PKG = "./cmd/lens/"

# the new guard
PRESENCE = "TestEveryAdminRegistrationReachesAnAuthorizationDecision"
ONLYMAIN = "TestAdminRoutesLiveOnlyInMainGo"
# the existing guard, whose blindness is the point
CLASSIFIED = "TestEveryAdminRouteIsClassified"
GATEMATCH = "TestClassificationMatchesTheGateAtEachRegistrationSite"
BRIEF = "TestBriefNamedRoutesAreOut"


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


# (id, description, edits, must_red, must_stay_green, note)
CONTROLS = [
    ("E1",
     "/v1/admin/lxc/grant (MINTS LXC) loses its requireAdmin wrapper",
     [(MAIN, 'authed.Post("/v1/admin/lxc/grant", requireAdmin(authManager, newAdminLXCGrantHandler(dualToken)))',
       'authed.Post("/v1/admin/lxc/grant", newAdminLXCGrantHandler(dualToken))')],
     PRESENCE, GATEMATCH,
     "newAdminLXCGrantHandler carries NO internal check — the wrapper was the only gate"),

    ("E1b", "the same deletion, seen by the OTHER existing classification test",
     [(MAIN, 'authed.Post("/v1/admin/lxc/grant", requireAdmin(authManager, newAdminLXCGrantHandler(dualToken)))',
       'authed.Post("/v1/admin/lxc/grant", newAdminLXCGrantHandler(dualToken))')],
     PRESENCE, CLASSIFIED, "and TestEveryAdminRouteIsClassified is green too"),

    ("E1c", "the same deletion, and even the brief's own named-routes test stays green",
     [(MAIN, 'authed.Post("/v1/admin/lxc/grant", requireAdmin(authManager, newAdminLXCGrantHandler(dualToken)))',
       'authed.Post("/v1/admin/lxc/grant", newAdminLXCGrantHandler(dualToken))')],
     PRESENCE, BRIEF, "the brief names lxc/grant explicitly and still cannot see this"),

    ("E2",
     "newAdjudicateHandler drops its admin check — SIX payout-revocation routes lose their gate at once",
     [(ADJ, "\t\tif err != nil || actx == nil || !actx.IsAdmin {\n\t\t\twriteJSONErr(rw, http.StatusForbidden, \"admin credentials required\")\n\t\t\treturn\n\t\t}",
       "\t\tif err != nil || actx == nil {\n\t\t\twriteJSONErr(rw, http.StatusForbidden, \"admin credentials required\")\n\t\t\treturn\n\t\t}")],
     PRESENCE, GATEMATCH,
     "pool-royalty, distill-royalty, held-mints x4 — every REVOKES A PAYOUT route in the tables"),

    ("E3",
     "the distill/preview handler VALUE's IsAdmin closure is hollowed out",
     [(MAIN, "\t\t\t\tactx, err := authManager.Authenticate(req)\n\t\t\t\tif err != nil || !actx.IsAdmin {\n\t\t\t\t\treturn false\n\t\t\t\t}\n\t\t\t\treturn true",
       "\t\t\t\t_, err := authManager.Authenticate(req)\n\t\t\t\tif err != nil {\n\t\t\t\t\treturn false\n\t\t\t\t}\n\t\t\t\treturn true")],
     PRESENCE, GATEMATCH,
     "the fourth mechanism — a gate held in a struct field, which no window-scan can see"),

    # ⚠ THE CONTRAST. The existing guard is asymmetric, not useless.
    ("E4",
     "an operatorREADABLE route is narrowed to requireAdmin — the case the EXISTING guard DOES catch",
     [(MAIN, 'r.Handle("/v1/admin/workspaces", requireAdminOrOperatorRead(authManager,',
       'r.Handle("/v1/admin/workspaces", requireAdmin(authManager,')],
     GATEMATCH, PRESENCE,
     "kept to show the asymmetry is the finding, not that the existing guard is worthless: "
     "when the WRAPPER IDENTITY changes it catches it; when the gate VANISHES it does not"),

    ("E5",
     "an admin route appears in a package-main file that is not main.go",
     [(SESSION, "func mountSessionKeyRoutes(",
       "// control: a path the classification guard's main.go-only parse cannot see.\nfunc mustNotBeInvisible() string { return \"/v1/admin/control/backdoor\" }\n\nfunc mountSessionKeyRoutes(")],
     ONLYMAIN, CLASSIFIED,
     "INVISIBLE, not unclassified — TestEveryAdminRouteIsClassified never sees the path at all"),

    ("E6",
     "break the sweep — the /v1/admin literal regex matches nothing",
     [(GATE, 'adminLiteralRe = regexp.MustCompile(`"(/v1/admin[^"]*)"`)',
       'adminLiteralRe = regexp.MustCompile(`"(/vZZZ/adminZZZ[^"]*)"`)')],
     PRESENCE, GATEMATCH,
     "the non-vacuity floor: a sweep that finds nothing reports no ungated route"),

    ("E7",
     "the authorization-decision token is changed to one no gate contains",
     [(GATE, 'const adminAuthDecisionToken = ".IsAdmin"',
       'const adminAuthDecisionToken = ".ZZZNoSuchDecisionZZZ"')],
     PRESENCE, GATEMATCH,
     "proves the verdict depends on FINDING the decision, not on the walk succeeding"),
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
                print(f"     anchor: {old[:100]!r}")
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
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build, "
                  f"so '{must_red} failed' proves nothing")
            missed.append((cid, "companion red"))
            continue
        if red_pass:
            print(f"   ✗ MISSED: {must_red} still PASSES with the defect reintroduced")
            missed.append((cid, "guard blind"))
        else:
            print(f"   ✓ CAUGHT by {must_red} — and {must_green} STAYED GREEN")
            caught.append(cid)

    print()
    print(f"CAUGHT {len(caught)}: {', '.join(caught) or '—'}")
    print(f"MISSED {len(missed)}: {', '.join(f'{c}({why})' for c, why in missed) or '—'}")
    print()
    print("READ THE 'STAYED GREEN' HALF OF E1, E1b, E1c, E2, E3 AND E5. That is the existing")
    print("classification guard passing over money routes whose gate had just been deleted.")
    if missed:
        sys.exit(1)


if __name__ == "__main__":
    main()
