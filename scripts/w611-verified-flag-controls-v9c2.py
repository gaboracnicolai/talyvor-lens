#!/usr/bin/env python3
"""w611-verified-flag-controls-v9c2.py — positive controls for the node-registry `verified` census
and for the executable record of what the flag means.

Same three rules as scripts/w64-credential-leak-controls.py and
scripts/w610-public-node-discovery-controls-v9c2.py, enforced mechanically:

  1. ASSERT THE ANCHOR COUNT BEFORE EDITING — a substitution that matches nothing edits zero bytes,
     and "the control did not apply" is byte-indistinguishable from "the guard caught it".
  2. EVERY CONTROL NAMES A TEST THAT MUST STAY GREEN — a mutation that breaks the build reds every
     test in the package, so a control with no green companion cannot tell caught from not-compiled.
  3. RESTORE AND PROVE IT — sha256 before the edit, re-checked after the revert.

D6-D9 drive real-Postgres tests; without LENS_TEST_DATABASE_URL they SKIP and are reported UNARMED
rather than counted as passes.

Run from the repo root:  python3 scripts/w611-verified-flag-controls-v9c2.py
"""

import hashlib
import os
import subprocess
import sys

COMPUTE = "internal/mining/compute_mining.go"
EMBED = "internal/mining/embedding_mining.go"
SNAPSHOT = "internal/controlplane/snapshot.go"
CENSUS = "internal/mining/registry_verified_census_test.go"
CPMAIN = "cmd/lens/main.go"

MINING = "./internal/mining/"

CENSUS_T = "TestRegistryVerifiedCensus"
CLASSIFIER_T = "TestRegistryCensusClassifierDoesNotCountQuotedVerified"
MEANING_T = "TestVerifiedMeansOnlyThatSomethingAnswered200"
REFUSES_T = "TestVerifiedIsNotSetWhenTheHostRefuses"
CAPABILITY_T = "TestVerifiedDoesNotProveTheNodeServesTheModelsItClaims"
EMBED_T = "TestVerifiedEmbeddingSiblingHasTheSameMeaning"
XTENANT_T = "TestListAvailableNodes_ReturnsEveryTenantsRowsWithURLAndOwner"


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
    r = subprocess.run(
        ["go", "test", pkg, "-run", "^" + name + "$", "-count=1", "-v"],
        capture_output=True, text=True,
    )
    return r.returncode == 0, ("--- SKIP: " + name) in r.stdout


# (id, description, [(file, old, new)], must_red, must_stay_green)
CONTROLS = [
    ("D1", "a NEW consumer starts gating on `verified` — the controlplane snapshot that feeds the routable fleet",
     [(SNAPSHOT, "\t\tFROM inference_nodes\n\t\tWHERE active = TRUE\n\t\tORDER BY last_seen_at DESC`)",
       "\t\tFROM inference_nodes\n\t\tWHERE active = TRUE AND verified = TRUE\n\t\tORDER BY last_seen_at DESC`)")],
     CENSUS_T, CLASSIFIER_T),

    # ⚠ THE ROW THAT IS THE FINDING. cache_nodes.verified is written by nothing today; if anything
    # starts writing it, the census must say so rather than absorb it.
    ("D2", "something starts writing cache_nodes.verified, which nothing has ever written",
     [(CPMAIN, "`UPDATE cache_nodes SET active = FALSE WHERE id = $1 AND workspace_id = $2`",
       "`UPDATE cache_nodes SET active = FALSE, verified = TRUE WHERE id = $1 AND workspace_id = $2`")],
     CENSUS_T, CLASSIFIER_T),

    ("D3", "the ONE gate disappears — ListAvailableNodes stops filtering on `verified`",
     [(COMPUTE, "WHERE verified = TRUE AND active = TRUE AND $1 = ANY(models)",
       "WHERE active = TRUE AND $1 = ANY(models)")],
     CENSUS_T, CLASSIFIER_T),

    ("D4", "break the sweep — the SQL-literal regex matches nothing, so the census covers zero statements",
     [(CENSUS, 'sqlLiteral     = regexp.MustCompile("`([^`]*)`")',
       'sqlLiteral     = regexp.MustCompile("ZZZ_NO_SUCH_LITERAL_ZZZ([^`]*)ZZZ")')],
     CENSUS_T, CLASSIFIER_T),

    # ⚠ THE CLASSIFIER CONTROL, ON A FALSE POSITIVE I ACTUALLY MADE. Stop excluding the quoted
    # 'verified' status value and confidential_minter.go's attestation JOIN counts as a second gate
    # on inference_nodes — a number that looks safer than the truth.
    ("D5", "the classifier counts node_attestations.attestation_status = 'verified' as a registry gate",
     [(CENSUS, 'clean := quotedVerified.ReplaceAllString(sql, "\'__STATUSVALUE__\'")',
       'clean := sql')],
     CLASSIFIER_T, XTENANT_T),

    ("D5b", "the same blinding, seen by the CENSUS rather than by the classifier control",
     [(CENSUS, 'clean := quotedVerified.ReplaceAllString(sql, "\'__STATUSVALUE__\'")',
       'clean := sql')],
     CENSUS_T, XTENANT_T),

    ("D6", "the probe starts authenticating — the 'no ownership proof' premise would be stale",
     [(COMPUTE, "\tresp, err := m.httpClient.Do(req)\n\tif err != nil {\n\t\treturn\n\t}\n\tdefer resp.Body.Close()\n\tif resp.StatusCode != http.StatusOK {\n\t\treturn\n\t}\n\tif m.pool == nil {\n\t\treturn\n\t}\n\t_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)",
       "\treq.Header.Set(\"Authorization\", \"Bearer control\")\n\tresp, err := m.httpClient.Do(req)\n\tif err != nil {\n\t\treturn\n\t}\n\tdefer resp.Body.Close()\n\tif resp.StatusCode != http.StatusOK {\n\t\treturn\n\t}\n\tif m.pool == nil {\n\t\treturn\n\t}\n\t_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)")],
     MEANING_T, CENSUS_T),

    ("D7", "the status check goes inert — any response verifies the node",
     [(COMPUTE, "\tif resp.StatusCode != http.StatusOK {\n\t\treturn\n\t}\n\tif m.pool == nil {\n\t\treturn\n\t}\n\t_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)",
       "\tif m.pool == nil {\n\t\treturn\n\t}\n\t_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)")],
     REFUSES_T, MEANING_T),

    ("D8", "the probe stops flipping the flag at all — the meaning tests must not pass on a dead probe",
     [(COMPUTE, "_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)",
       "_ = nodeID")],
     MEANING_T, REFUSES_T),

    ("D8b", "the same dead probe, seen by the capability test",
     [(COMPUTE, "_, _ = m.pool.Exec(ctx, `UPDATE inference_nodes SET verified = TRUE WHERE id = $1`, nodeID)",
       "_ = nodeID")],
     CAPABILITY_T, REFUSES_T),

    ("D9", "the embedding sibling's probe stops flipping the flag",
     [(EMBED, "_, _ = m.pool.Exec(ctx, `UPDATE embedding_nodes SET verified = TRUE WHERE id = $1`, nodeID)",
       "_ = nodeID")],
     EMBED_T, MEANING_T),
]


def main():
    if not os.getenv("LENS_TEST_DATABASE_URL"):
        print("⚠ LENS_TEST_DATABASE_URL is unset — D6-D9 drive real-PG tests that will SKIP.")
        print("  A skip is reported UNARMED, never counted as caught.\n")

    caught, missed, unarmed = [], [], []

    for cid, desc, edits, must_red, must_green in CONTROLS:
        print(f"── {cid}: {desc}")
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

        red_pass, red_skip = run_test(MINING, must_red)
        green_pass, green_skip = run_test(MINING, must_green)

        for path, s in originals.items():
            write(path, s)
            if sha(path) != shas[path]:
                print(f"   ✗ RESTORE FAILED for {path}")
                sys.exit(2)

        if red_skip:
            print(f"   ⚠ UNARMED: {must_red} SKIPPED (needs a database) — nothing was tested")
            unarmed.append((cid, must_red))
            continue
        if not green_skip and not green_pass:
            print(f"   ✗ COMPANION RED: {must_green} also failed — the mutation broke the build, "
                  f"so '{must_red} failed' proves nothing")
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
    if unarmed:
        print("\n⚠ exit 0 with UNARMED controls: re-run with LENS_TEST_DATABASE_URL to arm them.")


if __name__ == "__main__":
    main()
