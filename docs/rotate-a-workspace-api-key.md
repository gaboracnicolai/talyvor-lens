# Rotating a workspace API key

For a key a **running process** holds — the BFF's `tlv_ws_…` in `/etc/talyvor/bff.env`, a CI
credential, a customer's server-side integration.

> **Do not use `POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate` for this.**
> It is the obvious endpoint and it is the wrong one. Read the next section before running anything.

---

## Why the endpoint named `rotate` cannot rotate a key a service is holding

`RotateAPIKey` wraps `SELECT FOR UPDATE` → `INSERT` new → `DELETE` old in a **single transaction**.
Its source comment says:

> "The INSERT precedes the DELETE so no window exists where zero active keys exist for the
> workspace (the old key is live until the new one is safely persisted)."

That is **true about the transaction and false about the system**. Ordering `INSERT` before `DELETE`
is invisible outside the transaction — both rows change visibility at `COMMIT`, atomically. There is
no instant at which a *client* can authenticate with both keys. The old credential dies the moment
the call returns.

So a process still holding the old value is locked out for the whole of *write the new key to
config → restart the process*. For the BFF that interval is **every login**, and there is no second
admin path back in through the UI.

Measured, not argued: `internal/tenant/rotation_overlap_integration_test.go` asserts against a real
Postgres that the old key is refused immediately after `RotateAPIKey`, and that `CreateAPIKey` +
`RevokeAPIKey` is the only pair that opens an overlap window.

`RotateAPIKey` is not broken. Atomic replace is the right primitive for a key held by a **human**
who can paste the new value straight away, and its transaction is genuinely race-free (it fixed a
real TOCTOU where two concurrent rotates left two live keys). It is the wrong primitive for a key
held by a **process** — a property of the caller, not a bug in the function.

---

## The supported order

Two live keys per workspace is a supported state: `workspace_api_keys` has no unique constraint on
`workspace_id`, and `ValidateAPIKey` matches by key prefix, so both authenticate.

**Never delete first.** Deleting first locks everyone out, and for the BFF there is no way back in
through the UI.

### 1. Mint a second key — the old one keeps working

```bash
curl -sS -X POST "$LENS_URL/v1/workspaces/$WS_ID/api-keys" \
  -H "Authorization: Bearer $ADMIN_CREDENTIAL" \
  -H 'content-type: application/json' \
  -d '{"name":"bff-rotated-YYYY-MM-DD","scopes":["proxy"]}'
```

Copy `key` from the response. **It is not shown again.** Note the `id` — you need it in step 5, and
it is the *new* key's id, not the one you are retiring.

Record the OLD key's id first, so you are not reading it out of a list under pressure later:

```bash
curl -sS "$LENS_URL/v1/workspaces/$WS_ID/api-keys" \
  -H "Authorization: Bearer $ADMIN_CREDENTIAL" | jq '.[] | {id, name, key_prefix, created_at}'
```

### 2. Confirm the new key works *before* touching any config

```bash
curl -sS -o /dev/null -w '%{http_code}\n' "$LENS_URL/v1/auth/me" -H "Authorization: Bearer $NEW_KEY"
```

### 3. Write it to config

```bash
sudo cp /etc/talyvor/bff.env /etc/talyvor/bff.env.bak-$(date +%Y%m%d-%H%M%S)
sudo sed -i "s|^LENS_API_KEY=.*|LENS_API_KEY=$NEW_KEY|" /etc/talyvor/bff.env
```

Keep the backup until step 5 is done. Both keys are live, so rolling back is just restoring the file
and restarting.

### 4. Restart, then **verify a real authenticated login**

```bash
sudo systemctl restart talyvor-bff
```

> **Do not verify with `/api/version`.** That route needs no session and would report healthy while
> every login failed — the same shape as Track's `lens_healthy` defect. Log in as a real user, in a
> browser or with a scripted credential, and confirm you get a session back.

If login fails: restore the backup, restart, and stop. The old key is still valid — that is the
entire point of this order.

### 5. Only now revoke the old key

```bash
curl -sS -X DELETE "$LENS_URL/v1/workspaces/$WS_ID/api-keys/$OLD_KEY_ID" \
  -H "Authorization: Bearer $ADMIN_CREDENTIAL"
```

Revocation is immediate — no cache, no TTL (`internal/tenant/revocation_immediate_integration_test.go`).
Confirm the old key is dead, because a rotation that leaves the leaked credential live has achieved
nothing:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' "$LENS_URL/v1/auth/me" -H "Authorization: Bearer $OLD_KEY"
# expect 401
```

---

## The two key stores — check which one you are in

Lens has **two** key systems that share almost every word, and picking the wrong one produces a
credential that looks minted and authenticates nothing:

| | table | minted by | cached? |
|---|---|---|---|
| `auth.KeyStore` | `api_keys` | `POST /v1/api/keys` | yes, 5-minute TTL |
| `tenant.Store` | `workspace_api_keys` | `POST /v1/workspaces/{wsID}/api-keys` | **no** — every request hits the DB |

Workspace keys (`tlv_ws_…`), including the BFF's, are the **second** kind. A previous rotation
attempt minted into `api_keys` via `POST /v1/api/keys`; the key was inert, and rotating onto it
would have broken every login.

If the key you are holding starts `tlv_ws_`, use the `/v1/workspaces/{wsID}/api-keys` routes in this
document.

---

## Can a customer do this themselves?

Yes. All five routes are workspace-scoped behind `workspaceIsolationMiddleware`, so a workspace
owner can mint, list, revoke and rotate their own keys without an operator:

| | route |
|---|---|
| mint | `POST /v1/workspaces/{wsID}/api-keys` |
| list | `GET /v1/workspaces/{wsID}/api-keys` |
| revoke | `DELETE /v1/workspaces/{wsID}/api-keys/{keyID}` |
| atomic replace | `POST /v1/workspaces/{wsID}/api-keys/{keyID}/rotate` |
| usage | `GET /v1/workspaces/{wsID}/api-keys/{keyID}/usage` |

**What is missing is not a route, it is the warning.** Nothing in the API surface tells a customer
that `…/rotate` gives no overlap window, and its name actively suggests the opposite. A customer
rotating a key their own server is holding will reach for it and take an outage. That is a product
gap in the docs and in the endpoint's naming, and it is not fixed by this file alone.
