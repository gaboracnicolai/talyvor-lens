#!/usr/bin/env python3
"""w615-local-endpoints-contract-controls-v9c2.py — positive controls for the
GET /v1/local/endpoints contract-parity fix.

Same three rules as the earlier campaigns here:
  1. anchor count asserted BEFORE editing;
  2. every control names a companion test that must stay GREEN;
  3. sha256 before the edit, re-checked after the revert.

⚠ H4 IS THE ONE TO READ. It adds a property to the PUBLISHED SCHEMA and requires the
guard to fail because the RESPONSE does not carry it. Without that direction the guard
would be a one-way "don't ship extra fields" check, and a contract that promises
something the API never sends is the failure that looks like nothing.

Run from the repo root:  python3 scripts/w615-local-endpoints-contract-controls-v9c2.py
"""

import hashlib
import subprocess
import sys

HANDLER = "cmd/lens/local_endpoints_list_handler.go"
OPENAPI = "internal/api/openapi.go"
MAIN = "cmd/lens/main.go"
ROUTER = "internal/localrouter/multi.go"

PKG = "./cmd/lens/"

EXTRA = "TestLocalEndpointsList_ShipsNoFieldTheContractDoesNotDeclare"
MISSING = "TestLocalEndpointsList_KeepsEveryFieldTheContractDeclares"
OWNER = "TestLocalEndpointsList_DoesNotNameTheOwningTenant"
URLKEPT = "TestLocalEndpointsList_StillCarriesURL_ByDecisionNotOversight"
WIRED = "TestLocalEndpointsListRouteGoesThroughTheProjection"
VALUES = "TestLocalEndpointsList_CarriesTheSourceValuesNotJustTheKeys"


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
    ("H1", "revert the fix — the route writes the raw struct again",
     [(HANDLER, "writeJSONOK(w, http.StatusOK, publicLocalEndpointView(l.List()))",
       "writeJSONOK(w, http.StatusOK, l.List())")],
     EXTRA, URLKEPT,
     "three undeclared fields come back: workspace_id, last_check_at, active_count"),

    ("H1b", "the same revert, seen by the raw-bytes owner assertion",
     [(HANDLER, "writeJSONOK(w, http.StatusOK, publicLocalEndpointView(l.List()))",
       "writeJSONOK(w, http.StatusOK, l.List())")],
     OWNER, URLKEPT, "ws-A and ws-B are named to any authenticated caller again"),

    ("H2", "the owner is re-added to the projection under a DIFFERENT name",
     [(HANDLER, '\tErrorRate     float64  `json:"error_rate"`\n}',
       '\tErrorRate     float64  `json:"error_rate"`\n\tOwner         string   `json:"owner"`\n}'),
      (HANDLER, "AvgLatencyMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate,",
       "AvgLatencyMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate, Owner: e.WorkspaceID,")],
     OWNER, URLKEPT,
     "the raw-bytes assertion must catch a rename that a key-name check would miss"),

    ("H2b", "the same rename, seen by the contract-parity check",
     [(HANDLER, '\tErrorRate     float64  `json:"error_rate"`\n}',
       '\tErrorRate     float64  `json:"error_rate"`\n\tOwner         string   `json:"owner"`\n}'),
      (HANDLER, "AvgLatencyMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate,",
       "AvgLatencyMs: e.AvgLatencyMs, ErrorRate: e.ErrorRate, Owner: e.WorkspaceID,")],
     EXTRA, URLKEPT, "`owner` is undeclared too, so both directions of the guard see it"),

    # ⚠ THIS ANCHOR MOVED ONCE, AND THE MOVE IS THE POINT. The first draft deleted the
    # ASSIGNMENT (`Active: e.Active`), which leaves the FIELD on the struct — so the key is
    # still on the wire as `"active": false` and the contract check correctly passed. A
    # zeroed field is a present key. Deleting the FIELD is the real contract break, and the
    # value-fidelity gap that draft exposed is now closed by its own test (VALUES below).
    ("H3", "a DECLARED field is dropped from the response type entirely",
     [(HANDLER, '\tActive        bool     `json:"active"`\n', ""),
      (HANDLER, "MaxConcurrent: e.MaxConcurrent, Active: e.Active, Healthy: e.Healthy,",
       "MaxConcurrent: e.MaxConcurrent, Healthy: e.Healthy,")],
     MISSING, OWNER, "the contract promises `active` and the response stops carrying the key"),

    ("H3b", "a declared field is PRESENT but always zero — the key is there and the value is not",
     [(HANDLER, "MaxConcurrent: e.MaxConcurrent, Active: e.Active, Healthy: e.Healthy,",
       "MaxConcurrent: e.MaxConcurrent, Healthy: e.Healthy,")],
     VALUES, MISSING,
     "the contract check is key-level by design; this is the test that covers values"),

    # ⚠ THE DIRECTION THAT IS EASY TO FORGET. Widen the CONTRACT and the response must
    # be required to catch up; otherwise the guard only ever polices the response.
    ("H4", "the published schema grows a property the response does not carry",
     [(OPENAPI, '\t\t\t\t\t\t"error_rate":     map[string]any{"type": "number"},',
       '\t\t\t\t\t\t"error_rate":     map[string]any{"type": "number"},\n\t\t\t\t\t\t"queue_depth":    map[string]any{"type": "integer"},')],
     MISSING, OWNER,
     "a contract that promises what the API never sends is the failure that looks like nothing"),

    ("H5", "break the schema read — the guard compares against an empty contract",
     [(OPENAPI, '"LocalEndpoint": map[string]any{', '"LocalEndpointRenamed": map[string]any{')],
     EXTRA, OWNER,
     "an empty or missing contract accepts every response; the guard must refuse to run"),

    # ⚠ THE CUSTOM MARSHALLER. active_count is produced by LocalEndpoint.MarshalJSON,
    # not by a struct tag — a parity check written against struct tags would never see
    # it. This proves the guard reads the RESPONSE.
    ("H6", "the custom MarshalJSON folds an undeclared field into the raw shape",
     [(ROUTER, '\t\t*alias\n\t\tActiveCount int64 `json:"active_count"`',
       '\t\t*alias\n\t\tActiveCount int64 `json:"in_flight_now"`'),
      (HANDLER, "writeJSONOK(w, http.StatusOK, publicLocalEndpointView(l.List()))",
       "writeJSONOK(w, http.StatusOK, l.List())")],
     EXTRA, URLKEPT,
     "renaming it changes the wire and not the tags — the guard still catches it"),

    ("H7", "the route stops going through the projection",
     [(MAIN, 'authed.Get("/v1/local/endpoints", newLocalEndpointsListHandler(localRouterMulti))',
       'authed.Get("/v1/local/endpoints", func(w http.ResponseWriter, _ *http.Request) {\n'
       '\t\t\twriteJSONOK(w, http.StatusOK, localRouterMulti.List())\n'
       '\t\t})')],
     WIRED, EXTRA, "the projection can be perfect and unreached"),
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
