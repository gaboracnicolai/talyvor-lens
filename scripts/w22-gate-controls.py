#!/usr/bin/env python3
"""Positive controls for W2.2 merge 2 — the entity gate's empty-extraction hole.

Each control: assert the anchor count BEFORE the edit, apply it, observe the guard go RED, confirm
a companion test STAYS GREEN (so a build break cannot be read as a catch), restore the file and
verify it is sha256-identical.

⚠ THE SEAMS ARE THE POINT. An empty extraction reaches the pool through FOUR doors — the Go
comparison (discriminator.Match), the read path (GetPooled's refusal before the SQL), the pooled
upsert, and the doc2query variant upsert. C1..C4 revert one door each; a fix that closed only some
of them would leave the others green, which is how this class of defect survives.

⚠ C5 IS THE OPPOSITE DIRECTION and is the one that stops "fail closed" becoming "fail always":
it makes every canonical form unverifiable, so the gate refuses EVERYTHING. C1..C4 are all
satisfied by that, and only the floors catch it.

Requires LENS_TEST_DATABASE_URL (the cache tests build their own databases from migrations).
"""
import hashlib
import os
import pathlib
import subprocess
import sys

REPO = pathlib.Path.home() / 'talyvor-lens'
DSN = os.environ.get('LENS_TEST_DATABASE_URL',
                     'postgres://postgres:lens_test@localhost:55432/lens_test?sslmode=disable')

SUITES = {
    'discriminator': ['go', 'test', './internal/discriminator/', '-count=1', '-v'],
    'cache': ['go', 'test', './internal/cache/', '-count=1', '-v', '-run',
              'EmptyExtraction|WithVariants|EntityBearing|StillServes|RefusesAcross|NeitherPrompt|RefusesLegacyRow'],
}


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def verdicts():
    env = dict(os.environ, LENS_TEST_DATABASE_URL=DSN)
    v, out = {}, ''
    for cmd in SUITES.values():
        r = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True, env=env)
        out += r.stdout + r.stderr
    for line in out.splitlines():
        s = line.strip()
        for tag in ('--- PASS: ', '--- FAIL: '):
            if s.startswith(tag):
                v[s[len(tag):].split(' ')[0]] = tag.split()[1].rstrip(':')
    if not v:
        v['__BUILD__'] = 'BROKEN'
    return v, out


FLOORS = [
    'TestMatch_EntityBearingPairsStillMatch',
    'TestGetPooled_EntityBearingPairStillServesAfterTheFix',
    'TestGetPooled_StillServesWhenEntitiesMatch',
]

CONTROLS = [
    dict(
        name='C1 Match stops failing closed on an empty canon',
        path='internal/discriminator/discriminator.go',
        old='\tif !ca.Verifiable() || !cb.Verifiable() {\n\t\treturn false\n\t}\n',
        new='',
        must_fail=['TestMatch_EmptyExtractionIsNotAMatch'],
        must_pass=['TestMatch_EntityBearingPairsStillMatch'],
    ),
    dict(
        name='C2 the read path stops refusing before the SQL',
        path='internal/cache/semantic.go',
        old='\tcanon := discriminator.Canon(prompt)\n\tif !canon.Verifiable() {\n\t\treturn nil, "", "", 0, nil\n\t}\n',
        new='\tcanon := discriminator.Canon(prompt)\n',
        must_fail=['TestGetPooled_RefusesLegacyRowStoredWithEmptyStringDiscriminators'],
        must_pass=['TestGetPooled_EntityBearingPairStillServesAfterTheFix'],
    ),
    dict(
        name='C3 the pooled upsert stores the empty string again',
        path='internal/cache/semantic.go',
        old="VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), NULLIF($8, ''))",
        new="VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), $8)",
        must_fail=['TestSetPooled_EmptyExtractionStoresNULLNotEmptyString'],
        must_pass=['TestGetPooled_EntityBearingPairStillServesAfterTheFix'],
    ),
    dict(
        name='C4 the VARIANT upsert stores the empty string again',
        path='internal/cache/semantic.go',
        old="VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), NULLIF($8, ''), $9)",
        new="VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), $8, $9)",
        must_fail=['TestSetPooledWithVariants_EmptyExtractionStoresNULLOnEveryVariantRow'],
        must_pass=['TestSetPooledWithVariants_VariantInheritsOriginalDiscriminators'],
    ),
    dict(
        # ⚠ THE FAIL-ALWAYS CONTROL. Every control above is satisfied by a gate that refuses
        # everything; only the floors can tell the difference between "closed" and "shut".
        name='C5 the gate refuses EVERYTHING (fail-always)',
        path='internal/discriminator/discriminator.go',
        old='func (c Canonical) Verifiable() bool { return c != "" }',
        new='func (c Canonical) Verifiable() bool { return false }',
        must_fail=FLOORS,
        must_pass=['TestMatch_EmptyExtractionIsNotAMatch'],
    ),
]


def main():
    base, _ = verdicts()
    bad = {k: v for k, v in base.items() if v != 'PASS'}
    if bad:
        print('⚠ BASELINE NOT GREEN — controls would be meaningless:', bad)
        return 1
    print(f'baseline: {len(base)} tests, all PASS\n')

    caught = 0
    for c in CONTROLS:
        p = REPO / c['path']
        before, original = sha(p), p.read_text()
        n = original.count(c['old'])
        if n != 1:
            print(f"{c['name']:52} ⚠ ANCHOR COUNT {n}, EXPECTED 1 — NOT RUN")
            continue
        p.write_text(original.replace(c['old'], c['new'], 1))
        try:
            v, _ = verdicts()
        finally:
            p.write_text(original)
        assert sha(p) == before, f"{c['path']} not restored byte-identical"

        why = [f'{t}={v.get(t)}' for t in c['must_fail'] if v.get(t) != 'FAIL']
        why += [f'companion {t}={v.get(t)}' for t in c['must_pass'] if v.get(t) != 'PASS']
        ok = not why
        caught += ok
        print(f"{c['name']:52} {'CAUGHT' if ok else 'NOT CAUGHT — ' + '; '.join(why)}")

    print(f'\n{caught}/{len(CONTROLS)} caught; every file restored sha256-identical')
    return 0 if caught == len(CONTROLS) else 1


if __name__ == '__main__':
    sys.exit(main())
