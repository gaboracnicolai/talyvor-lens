# Rotating a Lens workspace key without an outage (W1.9.1)

Built 2026-08-26 (tab-h3n8). **If you are rotating a key a running service holds — the suite BFF,
a node, a CI job — use this page. Do not use `POST …/rotate`.**

## Which of the two rotations you want

| | `POST …/api-keys/{keyID}/rotate` | `POST …/api-keys/{keyID}/rotate/begin` (this page) |
|---|---|---|
| what it does | atomic replace, in one transaction | mints a replacement, **old key keeps working** |
| overlap | **none** | until you complete it |
| right for | a key a **person** holds and can paste now; a leaked key that must die this second | a key a **process** holds |
| wrong for | anything a service is presenting | a credential you need dead immediately |

`RotateAPIKey` is not broken and is not going away. Its transaction orders INSERT before DELETE so
the *workspace* is never left with zero keys — which is true, and is about the workspace. **The
holder is a different party:** both rows change visibility at COMMIT, so there is no instant in
which a client can authenticate with the old key and the new one. A service still presenting the old
credential is locked out for the whole of *write the new value into config → restart the process*.
`internal/tenant/rotation_overlap_integration_test.go` pins that against real Postgres.

## The sequence

**The order is the design. It must not invert.**

```
1. begin      →  new key minted.  BOTH KEYS AUTHENTICATE.
2. switch     →  put the new key where the holder reads it; restart if it needs restarting
3. verify     →  the holder makes one REAL authenticated request
4. complete   →  the old key is revoked.  REFUSED unless step 3 actually happened.
```

### 1. Begin

```sh
curl -sX POST -H "Authorization: Bearer $ADMIN" \
  "$LENS/v1/workspaces/$WS/api-keys/$KEY_ID/rotate/begin"
```

Returns `rotation_id`, the new `key` (**shown once**), and `old_key_still_valid: true`. Nothing has
been revoked. If a rotation is already open on this key you get **409** — complete or abandon that
one first, or the holder cannot be told which key to take.

### 2. Switch the holder

Write the new key wherever the holder reads it and restart it if it needs restarting. Take as long
as you need: the old key is still working, so there is no clock running.

### 3. Verify — with a request, not a probe

Have the holder make one **real authenticated request**. Then:

```sh
curl -s -H "Authorization: Bearer $ADMIN" \
  "$LENS/v1/workspaces/$WS/key-rotations/$ROTATION_ID"
```

`new_key_used: true` means the new key has authenticated something since this rotation opened.
`next_step` says what to do in words.

⚠ **A health check does not count and cannot be made to count.** A health route answers 200 with no
credential at all, so it is green precisely when the credential is missing — which is exactly the
`lens_healthy` defect this queue found in Track. The evidence used here is
`workspace_api_keys.last_used_at`, which `ValidateAPIKey` touches **only on a successful bcrypt
match**, and it is compared against the rotation's `started_at` rather than against NULL: "has this
key ever been used" would pass for any pre-existing key, "has it been used since we started" is the
question that means the holder switched.

### 4. Complete

```sh
curl -sX POST -H "Authorization: Bearer $ADMIN" \
  "$LENS/v1/workspaces/$WS/key-rotations/$ROTATION_ID/complete"
```

**412 Precondition Failed** means the new key has not authenticated anything yet. **Nothing was
revoked** — both keys are still live. Go back to step 2.

## If it goes wrong: abandon

```sh
curl -sX POST -H "Authorization: Bearer $ADMIN" \
  "$LENS/v1/workspaces/$WS/key-rotations/$ROTATION_ID/abandon"
```

**Abandon revokes the NEW key.** The key the holder is using is untouched, and the key can be
rotated again afterwards. Reach for this when the holder cannot take the new value — a deploy failed
halfway, the config did not land, the restart did not happen. You are already in trouble at that
point, which is exactly why abandon must not be the thing that takes the service down.

## What this does not do, said plainly

- **It does not rotate the global keys in `api_keys`.** Lens has two key stores and they are not
  duplicates: `api_keys` (`auth.KeyStore`) holds **global** keys and `workspace_api_keys`
  (`tenant.Store`) holds **workspace-scoped** ones; `auth.Manager` consults both, deliberately. The
  suite BFF's key is a workspace key, so this path is the one that covers it. Whether the two stores
  should ever become one is a question this merge does not answer and does not need to — see the
  queue entry.
- **It does not switch the holder for you.** Step 2 is yours. The value here is that steps 1 and 4
  cannot be run in the wrong order, and that step 4 cannot be run before step 3 has actually
  happened.
- **It does not expire an open rotation.** A rotation left open leaves two working keys, which is
  weaker than one. `GET …/key-rotations/{id}` reports `started_at`; if you want an alert on
  long-open rotations, that is the column to read. Choosing a maximum age is an operator decision
  and is not made here.
