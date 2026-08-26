# Model 2, step 4 — what a browser session can already reach, measured

**Status: a MEASUREMENT and a DECISION REQUEST. No behaviour changed in the commit that added this
file.** The only code it ships is `cmd/lens/session_credential_reach_test.go` (assertions) and
`scripts`-equivalent control harness `w461-sessionreach-controls-k7v3.py` in the queue directory.

## The premise this corrects

W4.6.1's step-3 note concluded, correctly on its own terms:

> Lens mounts inference at `POST /v1/proxy/{...}/*` and wraps every one in
> `auth.RequireScope(auth.ScopeProxy)`. The BFF's only Lens credential is the provisioned session
> token, minted with `provisionScopes = {analytics, keys}` — non-empty, and `proxy` is not in it.
> So every `/v1/proxy/*` call the BFF could make is refused 403.

and therefore:

> (a) add `proxy` to `provisionScopes` — **an authz decision on a money path: every signed-in
> browser session would then be able to drive inference against its workspace's balance.**

**The consequence in (a) is already true.** Not by a bug in the refusal — the refusal is real and
this repo now guards it — but because an adjacent, already-shipped route hands the same session a
*wider* credential on request.

## What was measured

`cmd/lens/session_credential_reach_test.go`, against a real Postgres, driving the real
`auth.AuthMiddleware` → `workspaceIsolationMiddleware` / `auth.RequireScope` chains:

| # | credential | route | result |
|---|-----------|-------|--------|
| 1 | session JWT, scopes `{analytics, keys}` | `POST /v1/proxy/openai/*` | **403** `missing scope proxy` |
| 2 | the same session JWT | `POST /v1/workspaces/{own}/api-keys`, `scopes:["proxy"]` | **201**, returns a `tlv_ws_…` key |
| 3 | the key from row 2 | `POST /v1/proxy/openai/*` | **200** |

Rows 1 and 3 are the same gate, the same process and the same request shape. The only thing that
changed is which credential sat in the header.

## Why row 2 succeeds

`auth.RequireScope` is called **exactly once** in the whole binary —
`auth.RequireScope(auth.ScopeProxy)` in `cmd/lens/main.go`, over the proxy routes. Six scope
constants are declared in `internal/auth/manager.go`; `admin`, `mint` and `operator_read` are
enforced by their own by-name checks, and `analytics` and `keys` are enforced by nothing at all.

So `provisionScopes = {analytics, keys}` **grants no capability**. Its only operative effect is to
make the scope set non-empty, which switches off `RequireScope`'s empty-set grandfather and thereby
*denies* `proxy`. The two scopes subtract; they do not add.

`POST /v1/workspaces/{wsID}/api-keys` carries no scope middleware — the only thing in front of it is
`workspaceIsolationMiddleware`, which asks *"do you own this workspace"*, never *"may you grant this
scope"*. A credential can therefore mint a credential carrying scopes it does not itself hold.

## It is not a theoretical path — it is the Keys screen

In talyvor-suite, the BFF's keys handler (`apps/bff/keys.go`, `handleMintKey`) decodes
`{name, scopes, expires_at}` from the browser, re-encodes them, and POSTs them to exactly that Lens
route with `Authorization: Bearer <the session's workspace token>`. The client's `scopes` are
forwarded verbatim. That handler's own comment already names the consequence: *"a console-minted key
that satisfies the proxy gate which spends the workspace's credit."*

## Why this changes step 4's brief rather than step 3's

The comparison is not "proxy scope vs no proxy scope". It is:

|  | session token **with** `proxy` | what the session can mint **today** |
|---|---|---|
| lifetime | dies with the JWT `exp` (≤ `MaxTokenTTL`) | none unless the caller asks for one |
| blast radius | that session | the whole workspace |
| survives sign-out | no | **yes** |
| held by the browser | no — the BFF keeps it server-side | **yes, returned in plaintext once** |
| revocable | by expiry | only by explicit user action on the Keys screen |

The credential reachable today is the **strictly wider** of the two. Step 4 — session-scoped keys —
is therefore not only the way to unblock step 3; it is the narrowing the Keys screen currently
lacks.

## The decision this needs (NOT taken here)

Closing row 2 means requiring the caller of the mint route to already hold the scopes it is
granting. **That would break the shipped Keys screen**: the BFF's session token holds
`{analytics, keys}`, so it could no longer mint the `proxy` key a user asks for — which is the
screen's entire purpose. So the fix is a product decision, not a defect repair, and it is not taken
here. The options, stated without a recommendation between the last two:

1. **Do nothing.** Accept that a signed-in session can mint a workspace-wide proxy key, and note
   that (a) — adding `proxy` to `provisionScopes` — is then a *narrowing* relative to the status
   quo, not a widening. This inverts step 3's blocker.
2. **Gate the mint route on a scope the session token holds** (`keys`), which makes `ScopeKeys` mean
   something for the first time but does not stop scope self-grant.
3. **Require caller-holds-scope, and give the Keys screen a different path to mint** — e.g. a
   step-up (re-auth) mint, or an operator/owner-only mint.
4. **Build step 4 and move the chat onto it**, leaving the Keys screen as it is. This does not close
   row 2; it means the *chat* never needs it.

Open questions step 4 must answer regardless, none decided here: what scopes a session key carries
beyond `proxy`, its TTL, whether sign-out revokes it, and whether it carries a per-session spend
bound distinct from the workspace balance and from the step-2 allowance.

## What the merged tests hold

- `TestProvisionedSessionToken_IsRefusedByTheProxyGate` — a guard that goes red the moment `proxy`
  is added to `provisionScopes`, i.e. the moment decision (a) is taken silently.
- `TestProxyScopedWorkspaceKey_IsAcceptedByTheProxyGate` — the control that stops the guard above
  passing against a gate that refuses everything.
- `TestMeasured_SessionTokenCanMintItselfAProxyScopedWorkspaceKey` — the finding. **Expected to go
  red when the escalation is closed**; its failure message says so, and says to delete it then.
- `TestMintRouteRegistrationCarriesNoScopeGate` / `TestRequireScopeHasExactlyOneCallSite` — source
  censuses over `main.go`, so the hand-composed fixtures above cannot drift into being more
  permissive than the router they model.

Positive controls: `w461-sessionreach-controls-k7v3.py`, **8/8 CAUGHT**, one mutation per behaviour,
each required to turn a named test red with a must-stay-green companion, sha256-verified restores.
It refuses to run without `LENS_TEST_DATABASE_URL` rather than scoring itself over skipped tests.

⚠ C5 caught a defect in the *test*, not the product, and it is the one worth reading: the first
fixture registered `workspaceIsolationMiddleware` with `chi.NewRouter().Use(...)`, where chi has not
yet matched the route and `chi.URLParam(r, "wsID")` returns `""` — so the isolation check
short-circuited and never ran. Denying every caller in `workspaceAuthorized` left the escalation test
green. `main.go` uses `r.Group(...)`, an inline mux whose middleware wraps the endpoint and therefore
sees populated params; the fixture now does the same. A fixture more permissive than the product is
how a proof of an authz gap ends up proving nothing.

---

# Step 4 as built — session-scoped keys

Shipped in the commit that adds `internal/sessionkey`, `migrations/0122_session_keys.sql` and
`cmd/lens/session_key_handler.go`. **Off by default**: `LENS_SESSION_KEYS_ENABLED` is false, the
three routes are not registered, and every `tlv_sk_` bearer is refused — a deployment that does not
opt in is byte-for-byte unchanged.

## The credential

| | workspace API key (what a session can mint today) | **session key** |
|---|---|---|
| lifetime | none unless the caller asks | **`expires_at` is `NOT NULL` in the schema** |
| scopes | chosen by the caller, stored on the row | **no scopes column exists**; `{proxy}` is a constant in `internal/auth` |
| blast radius | the whole workspace | the `(workspace, user)` pair |
| survives sign-out | yes | no — `DELETE /v1/auth/session-keys` is what sign-out calls |
| can mint another | yes | **no** — only a browser session JWT may mint |
| revocation | `DELETE` a row | `DELETE` a row, never cached, effective on the next request |

## The three bounds on its life

`min(what the caller asked for, LENS_SESSION_KEY_TTL, the caller's own remaining sign-in)`.

The third is the one that makes it a session key, and it is why `auth.AuthContext` gained
`ExpiresAt`. It is applied in one place — the handler — because a clamp spread over two layers is a
clamp nobody can state the value of. `MaxSessionKeyTTL` (12h) is deliberately shorter than
`auth.DefaultTokenTTL` (24h): a credential derived from a session must not have a longer maximum
than the session, or the derived credential is the wider one and the exercise inverts. A configured
value above the ceiling is **refused at startup, not clamped** — an operator who writes `720h` and
reads it back as `720h` must not get 12h.

## What it deliberately does NOT do

**No per-session spend bound.** The item asks whether a session key should carry one. It does not,
and the column is absent rather than present-and-unread. A column nothing reads is a claim that a
bound exists when it does not — the same shape as an inert scope. Wiring a bound means changing the
LXC admission gate, which is the **serving** path; step 2's own note says wiring a money gate as a
side effect of building a ledger is how a serving regression ships, and that argument applies here
unchanged. **That is the natural step 4b, and it is unclaimed.**

**It does not close the escalation measured above.** A session can still mint a workspace-wide
proxy key through the Keys screen. Step 4 means the *chat* never needs to — it does not remove the
option. Closing it is still the decision stated above.

## Controls

`w461-sessionkey-controls-k7v3.py` — **12/12 CAUGHT**. Every test was written red-first and went
green on implementation; that is the right order and it still does not prove an assertion has teeth,
so each behaviour gets a mutation that must turn a named test red.

⚠ Three controls first read as MISSED and none of them was a blind guard: the **companion** test
shared the mutation's blast radius, so a correct red was accompanied by a companion that could not
possibly stay green (adding `ScopeAdmin` to the session scope set *should* make the "refuses admin"
test fail). The rule the repair encodes: **a must-stay-green companion has to be a test the mutation
cannot touch.** The same mistake occurred once in `w461-sessionreach-controls-k7v3.py` (C4), which is
why it is written down here rather than only fixed.

⚠ D12 is worth reading for what it accidentally proved. Deleting the line that copies the JWT's `exp`
onto the AuthContext does not mint an unbounded key — the handler returns
`403 forbidden: the calling session has no remaining life`. The clamp **fails closed**, which is
what the code claimed and what the control confirmed.
