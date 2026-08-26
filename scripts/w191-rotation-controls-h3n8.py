#!/usr/bin/env python3
"""W1.9.1 positive controls — the two-phase key rotation.

Every guard passed on its first run, and this file is the only reason to believe any of them. Each
case mutates the PRODUCTION rotation path one property at a time and requires the NAMED marker, so
an assertion is pinned to the behaviour it claims to test rather than to "something broke".

⚠ THIS IS A CREDENTIAL PATH. Every mutation is restored and the restore is sha256-verified; a
control that leaves this file mutated is worse than no control at all.

Needs LENS_TEST_DATABASE_URL — the T-series SKIPS without it, and a control over a skipped test
proves nothing, so the script refuses to report a pass.
"""
import hashlib
import os
import pathlib
import re
import subprocess
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
ROT = ROOT / "internal" / "tenant" / "rotation.go"
MAIN = ROOT / "cmd" / "lens" / "main.go"
WIRE = ROOT / "cmd" / "lens" / "rotation_route_wiring_test.go"


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def run(pkg, pattern):
    r = subprocess.run(["go", "test", "-count=1", "-run", pattern, pkg],
                       cwd=ROOT, capture_output=True, text=True)
    return r.returncode, r.stdout + r.stderr


CASES = [
    ("N1", ROT, "BeginRotation revokes the old key too — i.e. it becomes RotateAPIKey with extra steps",
     [("\tif err := tx.Commit(ctx); err != nil {\n\t\treturn \"\", nil, nil, fmt.Errorf(\"tenant: commit rotation: %w\", err)\n\t}",
       "\tif _, err := tx.Exec(ctx, revokeKeySQL, old.ID); err != nil {\n\t\treturn \"\", nil, nil, err\n\t}\n\tif err := tx.Commit(ctx); err != nil {\n\t\treturn \"\", nil, nil, fmt.Errorf(\"tenant: commit rotation: %w\", err)\n\t}")],
     "[T1-OLD]", "./internal/tenant/", "TestT1"),

    ("N2", ROT, "the completion gate is dropped — an operator can revoke before the holder switched",
     [("\t\tif !usedSince(lastUsed, rot.StartedAt) {\n\t\t\treturn ErrNewKeyUnused\n\t\t}", "")],
     "[T2]", "./internal/tenant/", "TestT2"),

    ("N3", ROT, "the gate asks 'is last_used_at set' instead of 'was it used SINCE we started'",
     [("\treturn lastUsed != nil && lastUsed.After(startedAt)", "\treturn lastUsed != nil")],
     "[T4]", "./internal/tenant/", "TestT4"),

    ("N4", ROT, "FAIL-OPEN ORDERING: the old key is revoked BEFORE the precondition is checked",
     [("""		if !usedSince(lastUsed, rot.StartedAt) {
			return ErrNewKeyUnused
		}
		if _, err := tx.Exec(ctx, revokeKeySQL, rot.OldKeyID); err != nil {
			return fmt.Errorf("tenant: revoke old key: %w", err)
		}
		return nil""",
       """		if _, err := tx.Exec(ctx, revokeKeySQL, rot.OldKeyID); err != nil {
			return fmt.Errorf("tenant: revoke old key: %w", err)
		}
		if !usedSince(lastUsed, rot.StartedAt) {
			_ = tx.Commit(ctx)
			return ErrNewKeyUnused
		}
		return nil""")],
     "[T2-OLD]", "./internal/tenant/", "TestT2"),

    ("N5", ROT, "abandon revokes the OLD key — the outage reached from the direction of someone already in trouble",
     [("\t\tif _, err := tx.Exec(ctx, revokeKeySQL, rot.NewKeyID); err != nil {\n\t\t\treturn fmt.Errorf(\"tenant: revoke abandoned key: %w\", err)",
       "\t\tif _, err := tx.Exec(ctx, revokeKeySQL, rot.OldKeyID); err != nil {\n\t\t\treturn fmt.Errorf(\"tenant: revoke abandoned key: %w\", err)")],
     "[T5-OLD]", "./internal/tenant/", "TestT5"),

    ("N6", ROT, "the one-open-rotation-per-key check is dropped",
     [("\tcase err == nil:\n\t\treturn \"\", nil, nil, ErrRotationOpen",
       "\tcase err == nil:\n\t\t_ = openID")],
     "[T6]", "./internal/tenant/", "TestT6"),

    ("N7", ROT, "the workspace predicate is dropped from the rotation lookup — a cross-tenant revoke",
     [("WHERE id = $1 AND workspace_id = $2\nFOR UPDATE", "WHERE id = $1 AND $2 = $2\nFOR UPDATE")],
     "[T7]", "./internal/tenant/", "TestT7"),

    ("N8", ROT, "the replacement does not inherit the old key's scopes — a privilege change in a credential change's clothes",
     [("\t\tScopes:      append([]string{}, old.Scopes...),", "\t\tScopes:      []string{\"proxy\"},")],
     "[T1-SCOPES]", "./internal/tenant/", "TestT1"),

    # ── the wiring guard ──
    ("RW-C1", MAIN, "the completion route is moved off the isolated group",
     [('authed.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/complete"',
       'r.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/complete"')],
     "[RW2]", "./cmd/lens/", "TestRotationRoutes"),

    ("RW-C2", MAIN, "the abandon route is never registered",
     [('authed.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon"',
       'authed.Post("/v1/workspaces/{wsID}/key-rotations/{rotationID}/abandon-DISABLED"')],
     "[RW1]", "./cmd/lens/", "TestRotationRoutes"),

    ("V1", WIRE, "VACUITY: the walk reads a registration shape this router does not use",
     [('		case "Get", "Post", "Put", "Delete", "Patch", "Head":',
       '		case "GetXX", "PostXX", "PutXX", "DeleteXX", "PatchXX", "HeadXX":')],
     "[RW3]", "./cmd/lens/", "TestRotationRoutes"),
]


def main():
    if not os.environ.get("LENS_TEST_DATABASE_URL"):
        print("LENS_TEST_DATABASE_URL is not set — the T-series SKIPS and a control over a skipped "
              "test proves nothing. Refusing to report a pass.")
        return 2

    before = {p: sha(p) for p in (ROT, MAIN, WIRE)}
    for pkg, pat in (("./internal/tenant/", "TestT"), ("./cmd/lens/", "TestRotationRoutes")):
        code, out = run(pkg, pat)
        if code != 0:
            print(f"BASELINE NOT GREEN for {pkg} {pat}:\n{out[-2000:]}")
            return 1
    print("baseline: rotation store + wiring guards GREEN\n")

    caught, missed = [], []
    for cid, target, why, edits, marker, pkg, pat in CASES:
        original = target.read_text()
        if not all(original.count(o) == 1 for o, _ in edits):
            print(f"{cid} ANCHOR MISS: sites occur {[original.count(o) for o, _ in edits]}, want all 1 "
                  f"— the control never ran. ({why})")
            missed.append(cid)
            continue
        try:
            mutated = original
            for o, n in edits:
                mutated = mutated.replace(o, n, 1)
            target.write_text(mutated)
            code, out = run(pkg, pat)
            if code == 0:
                print(f"{cid} NOT CAUGHT — {why}: still GREEN.")
                missed.append(cid)
            elif marker not in out:
                tags = sorted(set(re.findall(r"\[[A-Z0-9-]+\]", out)))
                print(f"{cid} WRONG GUARD — {why}: red, but {marker} never fired. Saw {tags}.")
                missed.append(cid)
            else:
                print(f"{cid} CAUGHT by {marker} — {why}")
                caught.append(cid)
        finally:
            target.write_text(original)
            if sha(target) != before[target]:
                print(f"{cid} RESTORE FAILED on {target} — STOPPING. This is a credential path.")
                return 2

    for f in before:
        if sha(f) != before[f]:
            print(f"RESTORE DRIFT on {f}")
            return 2
    ok = True
    for pkg, pat in (("./internal/tenant/", "TestT"), ("./cmd/lens/", "TestRotationRoutes")):
        code, _ = run(pkg, pat)
        ok = ok and code == 0
    print(f"\nrestored: sha256 matches for all {len(before)} touched files; re-run "
          f"{'GREEN' if ok else 'RED'}")
    print(f"CONTROLS: {len(caught)}/{len(CASES)} CAUGHT" + (f"; MISSED {missed}" if missed else ""))
    return 0 if not missed and ok else 1


if __name__ == "__main__":
    sys.exit(main())
