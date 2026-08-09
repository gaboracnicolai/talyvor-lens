#!/usr/bin/env python3
"""w64-credential-leak-controls.py — the positive-control campaign for the upstream credential leak.

Every control is a MUTATION applied to the working tree, run, observed, and reverted. Three rules,
each learned the hard way in this queue and each enforced mechanically here:

  1. ASSERT THE ANCHOR COUNT BEFORE EDITING. A substitution that matches nothing edits zero bytes,
     and "the control did not apply" is byte-indistinguishable from "the guard caught it". Every
     control below asserts its anchor occurs exactly once before it is allowed to run.

  2. EVERY CONTROL NAMES A TEST THAT MUST STAY GREEN. A mutation that breaks the build reds every
     test in the package, including the one the control claims to have tripped — so a control with
     no green companion cannot tell "caught" from "nothing compiled".

  3. RESTORE AND PROVE IT. Each file's sha256 is taken before the edit and re-checked after the
     revert.

Run from the repo root:  python3 scripts/w64-credential-leak-controls.py
"""

import hashlib
import subprocess
import sys

PROXY = "internal/proxy/proxy.go"
STREAM = "internal/proxy/stream.go"
AUTHHDR = "internal/auth/credential_headers.go"
CONFIG = "internal/inference/config.go"
GUARD = "internal/proxy/upstream_credential_leak_test.go"

LEAK = "TestForward_ClientCredentialNeverReachesUpstream"
STREAMLEAK = "TestStream_ClientCredentialNeverReachesUpstream"
COPYLOOPS = "TestHeaderCopyLoops_AllClassified"
DETECTOR = "TestLeakDetector_SeesALeak"
PROVIDERS = "TestProviderProbes_CoverConfigForSwitch"
HEADERSET = "TestCredentialHeaderSet_MatchesAuthSource"
CALLSITES = "TestRunUpstreamCallSites_CarryNoRawInboundHeaders"
ORACLE = "TestForward_HeaderAuthBehavior_Characterization"


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
    """Returns True when the named test PASSES."""
    r = subprocess.run(
        ["go", "test", "./internal/proxy/", "-run", "^" + name + "$", "-count=1"],
        capture_output=True, text=True,
    )
    return r.returncode == 0


# Each control: (id, description, [(file, old, new)], must_red, must_stay_green)
CONTROLS = [
    # ⚠ C1 IS TWO EDITS ON PURPOSE, AND THE FIRST RUN OF THIS HARNESS IS WHY. Reverting only the
    # call site leaves `internal/auth` imported and unused, which is a COMPILE error in Go — every
    # test in the package reds, including the oracle that is supposed to stay green, and the control
    # can no longer tell "the guard caught it" from "nothing compiled". The companion assertion is
    # what surfaced that; the control now removes the import too, so the revert is a real revert.
    ("C1", "revert the fix — forward hands RunUpstream the raw inbound headers again",
     [(PROXY, "auth.StripCredentialHeaders(r.Header))", "r.Header)"),
      (PROXY, '\t"github.com/talyvor/lens/internal/auth"\n', "")],
     LEAK, ORACLE),

    ("C2", "hollow the stripper — it clones and deletes nothing",
     [(AUTHHDR, "\t\tout.Del(name)\n", "\t\t_ = name\n")],
     LEAK, CALLSITES),

    ("C3", "narrow auth.CredentialHeaders to Authorization alone",
     [(AUTHHDR,
       'var CredentialHeaders = []string{"Authorization", "X-Talyvor-Key", "X-API-Key"}',
       'var CredentialHeaders = []string{"Authorization"}')],
     LEAK, ORACLE),

    ("C3b", "the same narrowing, seen by the DERIVED header-set guard rather than by behaviour",
     [(AUTHHDR,
       'var CredentialHeaders = []string{"Authorization", "X-Talyvor-Key", "X-API-Key"}',
       'var CredentialHeaders = []string{"Authorization"}')],
     HEADERSET, ORACLE),

    ("C4", "break the parse path — the header-set guard must FAIL, not silently sweep nothing",
     [(GUARD, '"../auth/manager.go":    {"extractCredential"}',
       '"../auth/manager_GONE.go":    {"extractCredential"}')],
     HEADERSET, LEAK),

    ("C5", "ConfigFor grows an unprobed provider case",
     [(CONFIG, '\tcase "bedrock":\n',
       '\tcase "brandnew":\n\t\treturn ProviderConfig{name: "brandnew"}\n\tcase "bedrock":\n')],
     PROVIDERS, LEAK),

    ("C6", "ConfigFor loses a probed provider case (renamed out from under the table)",
     [(CONFIG, '\tcase "groq":\n', '\tcase "groqq":\n')],
     PROVIDERS, ORACLE),

    ("C7", "a SECOND RunUpstream call site appears, passing raw inbound headers",
     [(PROXY, "// upstreamProviderLabel guards the provider metric label",
       "func (p *Proxy) controlProbeUpstream(ctx context.Context, r *http.Request, body []byte, cfg providerConfig) {\n"
       "\t_, _, _, _ = inference.RunUpstream(ctx, p.httpClient, p.retryConfig, \"http://x\", cfg.ApplyAuth, body, r.Header)\n"
       "}\n\n// upstreamProviderLabel guards the provider metric label")],
     CALLSITES, LEAK),

    ("C8", "blind the probe — it stops putting the credentials on the outbound request",
     [(GUARD, "\tfor name, value := range leakSentinels {\n\t\tr.Header.Set(name, value)\n\t}\n\tr.Header.Set(benignClientHeader",
       "\tr.Header.Set(benignClientHeader")],
     LEAK, DETECTOR),

    ("C9", "break the detector — contains() never finds anything",
     [(GUARD, "func (c upstreamCapture) contains(needle string) (where string, found bool) {\n",
       "func (c upstreamCapture) contains(needle string) (where string, found bool) {\n\tif needle != \"\" {\n\t\treturn \"\", false\n\t}\n")],
     DETECTOR, ORACLE),

    ("C10", "over-broad fix — the seam also strips a benign client header",
     [(AUTHHDR,
       'var CredentialHeaders = []string{"Authorization", "X-Talyvor-Key", "X-API-Key"}',
       'var CredentialHeaders = []string{"Authorization", "X-Talyvor-Key", "X-API-Key", "X-Client-Trace"}')],
     LEAK, DETECTOR),

    ("C11", "strip the credentials and never re-apply the provider's own auth",
     [(CONFIG, '\t\t\t\treq.Header.Set("Authorization", "Bearer "+ep.OpenAIKey)\n',
       '\t\t\t\t_ = ep.OpenAIKey\n')],
     LEAK, DETECTOR),

    # ⚠ C12 IS THE ONE THAT MATTERS MOST. The first version of this fix landed only at
    # proxy.forward and left the streaming seam leaking; the forward test was green throughout.
    # This control reproduces that state exactly: the streaming assertion reds while the forward
    # assertion stays green, which is the shape of the mistake, pinned.
    ("C12", "revert the STREAMING half — forward is fixed, streaming still leaks",
     [(STREAM, "range auth.StripCredentialHeaders(r.Header) {", "range r.Header {"),
      (STREAM, '\t"github.com/talyvor/lens/internal/auth"\n', "")],
     STREAMLEAK, LEAK),

    ("C13", "the same streaming revert, seen by the STRUCTURAL seam guard rather than by behaviour",
     [(STREAM, "range auth.StripCredentialHeaders(r.Header) {", "range r.Header {"),
      (STREAM, '\t"github.com/talyvor/lens/internal/auth"\n', "")],
     COPYLOOPS, LEAK),

    ("C14", "a THIRD, unclassified header-copy loop appears",
     [(PROXY, "// upstreamProviderLabel guards the provider metric label",
       "func controlCopyHeaders(dst *http.Request, src http.Header) {\n"
       "\tfor name, values := range src {\n"
       "\t\tfor _, v := range values {\n"
       "\t\t\tdst.Header.Add(name, v)\n"
       "\t\t}\n"
       "\t}\n"
       "}\n\n// upstreamProviderLabel guards the provider metric label")],
     COPYLOOPS, STREAMLEAK),

    ("C15", "break the seam guard's walk root — it must FAIL, not sweep nothing and pass",
     [(GUARD, 'filepath.Walk("../..", func', 'filepath.Walk("../../no-such-root", func')],
     COPYLOOPS, CALLSITES),
]


def main():
    failures = []
    for cid, desc, edits, must_red, stay_green in CONTROLS:
        befores = {}
        # RULE 1 — assert EVERY anchor before touching a single byte. All asserts precede the
        # single write: a half-applied control is a mutation nobody described.
        ok = True
        staged = {}
        for path, old, _new in edits:
            if path not in befores:
                befores[path] = (read(path), sha(path))
                staged[path] = befores[path][0]
        for path, old, new in edits:
            n = staged[path].count(old)
            if n != 1:
                print(f"{cid}: ANCHOR COUNT {n} (want 1) in {path} — control NOT applied")
                failures.append(f"{cid}: anchor")
                ok = False
                break
            # Stage cumulatively so a multi-edit control on one file keeps every edit.
            staged[path] = staged[path].replace(old, new, 1)
        if not ok:
            continue

        for path, content in staged.items():
            write(path, content)

        red = not run_test(must_red)
        green = run_test(stay_green)

        for path, (src, _h) in befores.items():
            write(path, src)
        for path, (_src, h) in befores.items():
            if sha(path) != h:
                print(f"{cid}: RESTORE FAILED for {path}")
                failures.append(f"{cid}: restore")

        verdict = "OK " if (red and green) else "BAD"
        print(f"{verdict} {cid}: {desc}")
        print(f"       {must_red} red={red}   {stay_green} still green={green}")
        if not red:
            failures.append(f"{cid}: {must_red} stayed GREEN under the mutation")
        if not green:
            failures.append(f"{cid}: {stay_green} went red — cannot tell 'caught' from 'nothing compiled'")

    print()
    if failures:
        print("CONTROL CAMPAIGN FAILED:")
        for f in failures:
            print("  -", f)
        return 1
    print(f"all {len(CONTROLS)} controls fired, each with its companion still green, all files restored sha256-identical")
    return 0


if __name__ == "__main__":
    sys.exit(main())
