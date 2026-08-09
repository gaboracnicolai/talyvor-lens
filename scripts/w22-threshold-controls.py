#!/usr/bin/env python3
"""Positive controls for W2.2 merge 1 (the threshold raise).

Each control: assert the anchor count BEFORE the edit, apply it, observe the guard go RED and
NAME ITS OWN DEFECT, confirm a companion test STAYS GREEN (so a compile error cannot be read as a
caught mutation), then restore the file and verify it is sha256-identical.
"""
import hashlib
import pathlib
import subprocess
import sys

REPO = pathlib.Path.home() / 'talyvor-lens'
RUN = ['go', 'test', './internal/config/', '-count=1', '-v', '-run',
       'SemanticThreshold|DecidedThreshold|EnvStillOverrides|EnvExampleAgrees|ConscheckBinds']


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def verdicts():
    """Return {test_name: 'PASS'|'FAIL'} plus the raw output."""
    r = subprocess.run(RUN, cwd=REPO, capture_output=True, text=True)
    out = r.stdout + r.stderr
    v = {}
    for line in out.splitlines():
        s = line.strip()
        for tag in ('--- PASS: ', '--- FAIL: '):
            if s.startswith(tag):
                v[s[len(tag):].split(' ')[0]] = tag.split()[1].rstrip(':')
    # ⚠ a build failure yields NO verdicts at all; that must not read as "caught"
    if not v:
        v['__BUILD__'] = 'BROKEN'
    return v, out


CONTROLS = [
    dict(
        name='C1 conscheck re-hardcodes the threshold',
        path='cmd/conscheck/main.go',
        old='\tth := config.DefaultSemanticThreshold // the value production boots with, not a copy of it\n',
        new='\tth := 0.92 // config.DefaultSemanticThreshold — production\n',
        must_fail='TestConscheckBindsToTheSharedConstant',
        must_pass='TestDefaultSemanticThreshold_IsTheDecidedValue',
        says='does not reference config.DefaultSemanticThreshold',
    ),
    dict(
        name='C2 the env override is severed',
        path='internal/config/config.go',
        old='\tif v := os.Getenv("LENS_SEMANTIC_THRESHOLD"); v != "" {',
        new='\tif v := os.Getenv("LENS_SEMANTIC_THRESHOLD_DISABLED"); v != "" {',
        must_fail='TestLoad_EnvStillOverrides',
        must_pass='TestLoad_DefaultsToDecidedThreshold',
        says='env override no longer reaches',
    ),
    dict(
        name='C3 the struct literal drifts off the constant',
        path='internal/config/config.go',
        old='\t\tSemanticThreshold: DefaultSemanticThreshold,\n',
        new='\t\tSemanticThreshold: 0.92,\n',
        must_fail='TestLoad_DefaultsToDecidedThreshold',
        must_pass='TestEnvExampleAgreesWithTheDefault',
        says='drifted from',
    ),
    dict(
        name='C4 .env.example keeps the old value',
        path='.env.example',
        old='LENS_SEMANTIC_THRESHOLD=0.98\n',
        new='LENS_SEMANTIC_THRESHOLD=0.92\n',
        must_fail='TestEnvExampleAgreesWithTheDefault',
        must_pass='TestLoad_DefaultsToDecidedThreshold',
        says='the copy operators deploy',
    ),
    dict(
        name='C5 the constant itself is reverted',
        path='internal/config/config.go',
        old='const DefaultSemanticThreshold = 0.98',
        new='const DefaultSemanticThreshold = 0.92',
        must_fail='TestDefaultSemanticThreshold_IsTheDecidedValue',
        must_pass='TestLoad_EnvStillOverrides',
        says="want 0.98",
    ),
]


def main():
    base, _ = verdicts()
    if any(x != 'PASS' for x in base.values()):
        print('⚠ BASELINE IS NOT GREEN — controls would be meaningless:', base)
        return 1
    print(f'baseline: {len(base)} tests, all PASS\n')

    caught = 0
    for c in CONTROLS:
        p = REPO / c['path']
        before, original = sha(p), p.read_text()
        n = original.count(c['old'])
        if n != 1:
            print(f"{c['name']:48} ⚠ ANCHOR COUNT {n}, EXPECTED 1 — NOT RUN")
            continue
        p.write_text(original.replace(c['old'], c['new'], 1))
        try:
            v, out = verdicts()
        finally:
            p.write_text(original)
        assert sha(p) == before, f"{c['path']} not restored byte-identical"

        got_fail = v.get(c['must_fail'])
        got_pass = v.get(c['must_pass'])
        said = c['says'] in out
        ok = got_fail == 'FAIL' and got_pass == 'PASS' and said
        caught += ok
        why = []
        if got_fail != 'FAIL':
            why.append(f"{c['must_fail']}={got_fail}")
        if got_pass != 'PASS':
            why.append(f"companion {c['must_pass']}={got_pass}")
        if not said:
            why.append(f'message did not say {c["says"]!r}')
        print(f"{c['name']:48} {'CAUGHT' if ok else 'NOT CAUGHT — ' + '; '.join(why)}")

    print(f'\n{caught}/{len(CONTROLS)} caught; every file restored sha256-identical')
    return 0 if caught == len(CONTROLS) else 1


if __name__ == '__main__':
    sys.exit(main())
