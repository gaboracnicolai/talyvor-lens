#!/usr/bin/env python3
"""Positive controls for W2.2 merge 3 — the pooled WRITE's unservable spend.

Each control: assert the anchor count BEFORE the edit, apply it, observe the guard go RED, confirm
a companion test STAYS GREEN (so a build break cannot be read as a catch), restore the file and
verify it is sha256-identical.

⚠ TWO OF THE FOUR CONTROL THE SCOPE, NOT THE REFUSAL. The refusal is easy to get right and easy to
apply to the wrong seam. C3 moves it onto the EXACT pooled write (which is keyed on byte-identical
prompt text and needs no entity gate) and C4 moves it onto the PRIVATE semantic write; both are
plausible-looking edits that break real behaviour, and each has to be caught by a different test.

⚠ C2 IS THE FAIL-ALWAYS DIRECTION — it refuses every pooled write. The defect test is SATISFIED by
that, so only the floor can tell "refuse the unservable" from "never pool anything".

No database and no embedder: the semantic cache is driven through SemanticDB with a recording fake,
so these run wherever `go test` runs.
"""
import hashlib
import os
import pathlib
import subprocess
import sys

REPO = pathlib.Path.home() / 'talyvor-lens'

SUITES = {
    'proxy': ['go', 'test', './internal/proxy/', '-count=1', '-v', '-run',
              'TestStoreCaches_|TestPooling_'],
}

DEFECT = 'TestStoreCaches_UnverifiablePromptBuysNoPooledEmbedding'
FLOOR_POOLS = 'TestStoreCaches_VerifiablePromptStillPoolsAndStillPaysExactlyOnce'
FLOOR_PRIVATE = 'TestStoreCaches_PrivateSemanticWriteSurvivesAnUnverifiablePrompt'
FLOOR_EXACT = 'TestPooling_AllOn_CrossTenantHit_UnverifiablePrompt'
# ⚠ THE OLD CROSS-TENANT FLOOR IS BLIND TO C3 AND IS LISTED AS A COMPANION TO PROVE IT: its
# fixture "what is 2+2" canonicalises to `num:2`, which IS verifiable, so gating the exact
# write leaves it green. A control is only evidence if the fixture can tell the mutation apart.
BLIND_EXACT = 'TestPooling_AllOn_CrossTenantHit'

POOLED_SEMANTIC = ('\t\tif p.poolGate.DecidePoolableOnWrite(ctx, wsID) && '
                   'discriminator.Canon(rawPrompt).Verifiable() {')
POOLED_EXACT = ('\t\tif p.poolGate.DecidePoolableOnWrite(ctx, wsID) {\n'
                '\t\t\t_ = p.exact.SetWithOwner(ctx, provider, model, pooledPromptKey(rawPrompt), wsID, response)\n'
                '\t\t}')
PRIVATE_EMBED = '\t\tif vec, err := p.embedder.Embed(ctx, cachePrompt); err == nil {'


def sha(p):
    return hashlib.sha256(p.read_bytes()).hexdigest()


def verdicts():
    v, out = {}, ''
    for cmd in SUITES.values():
        r = subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)
        out += r.stdout + r.stderr
    for line in out.splitlines():
        s = line.strip()
        for tag in ('--- PASS: ', '--- FAIL: '):
            if s.startswith(tag):
                v[s[len(tag):].split(' ')[0]] = tag.split()[1].rstrip(':')
    if not v:
        v['__BUILD__'] = 'BROKEN'
    return v, out


CONTROLS = [
    dict(
        name='C1 the write stops refusing an unverifiable prompt',
        path='internal/proxy/proxy.go',
        old=POOLED_SEMANTIC,
        # ⚠ KEEPS THE Canon CALL so the discriminator import stays used. Deleting the clause
        # outright breaks the BUILD, and a build break is not a catch — it produces no
        # verdicts at all, which this harness reports as NOT CAUGHT.
        new='\t\tif p.poolGate.DecidePoolableOnWrite(ctx, wsID) && '
            '(discriminator.Canon(rawPrompt).Verifiable() || true) {',
        must_fail=[DEFECT],
        must_pass=[FLOOR_POOLS, FLOOR_PRIVATE, FLOOR_EXACT],
    ),
    dict(
        # ⚠ THE FAIL-ALWAYS CONTROL. The defect test passes happily under this; only the floor
        # distinguishes a gate that is closed from one that is shut.
        name='C2 the write refuses EVERYTHING (fail-always)',
        path='internal/proxy/proxy.go',
        old=POOLED_SEMANTIC,
        new='\t\tif p.poolGate.DecidePoolableOnWrite(ctx, wsID) && '
            'discriminator.Canon(rawPrompt).Verifiable() && false {',
        must_fail=[FLOOR_POOLS],
        must_pass=[DEFECT, FLOOR_PRIVATE, FLOOR_EXACT],
    ),
    dict(
        # ⚠ WRONG SEAM #1: the same refusal, applied to the EXACT pooled write. It looks more
        # thorough and it drops real cross-tenant hits on byte-identical prompts.
        name='C3 the refusal spreads to the EXACT pooled write',
        path='internal/proxy/proxy.go',
        old=POOLED_EXACT,
        new=('\t\tif p.poolGate.DecidePoolableOnWrite(ctx, wsID) && '
             'discriminator.Canon(rawPrompt).Verifiable() {\n'
             '\t\t\t_ = p.exact.SetWithOwner(ctx, provider, model, pooledPromptKey(rawPrompt), wsID, response)\n'
             '\t\t}'),
        must_fail=[FLOOR_EXACT],
        must_pass=[DEFECT, FLOOR_POOLS, FLOOR_PRIVATE, BLIND_EXACT],
    ),
    dict(
        # ⚠ WRONG SEAM #2: the refusal applied to the PRIVATE semantic write, which would be a
        # silent cache regression on every workspace's own entries.
        name='C4 the refusal spreads to the PRIVATE semantic write',
        path='internal/proxy/proxy.go',
        old=PRIVATE_EMBED,
        new=('\t\tif vec, err := p.embedder.Embed(ctx, cachePrompt); err == nil && '
             'discriminator.Canon(rawPrompt).Verifiable() {'),
        must_fail=[FLOOR_PRIVATE],
        must_pass=[DEFECT, FLOOR_POOLS, FLOOR_EXACT],
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
            print(f"{c['name']:56} ⚠ ANCHOR COUNT {n}, EXPECTED 1 — NOT RUN")
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
        print(f"{c['name']:56} {'CAUGHT' if ok else 'NOT CAUGHT — ' + '; '.join(why)}")

    print(f'\n{caught}/{len(CONTROLS)} caught; every file restored sha256-identical')
    return 0 if caught == len(CONTROLS) else 1


if __name__ == '__main__':
    sys.exit(main())
