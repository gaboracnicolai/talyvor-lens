# Provider-secret envelope encryption (W6.10)

The mechanism that lets Lens take custody of a **customer's** upstream provider credential, and the
operator runbook for the key that protects it. Built 2026-08-26 (W6.10, tab-h3n8) against main
`dd1bb44`.

## What shipped, and what deliberately did not

**Shipped:** `internal/envelope` (the crypto), `LENS_PROVIDER_SECRET_KEK` (the operator key, with
boot validation), its compose/`.env.example` declarations, and `Config.ProviderSecretsEnabled()` —
the single gate every future custody path must consult.

**Not shipped, on purpose:** there is no table, no route and no handler that stores a provider
secret. W6.10's own words are *"NOTHING ACCEPTS A PROVIDER SECRET UNTIL THIS SHIPS"* — this is the
"this". W6.4 / W6.8 build the store, and the contract they must honour is in the last section.

## Why envelope and not a single static key

W6.10 says to read how `talyvor-track` does it and mirror it *"unless there is a measured reason not
to"*. Track's `internal/integrations/crypto.go` is a **single static AES-256-GCM key** supplied
whole via `TRACK_INTEGRATION_ENCRYPTION_KEY`.

The measured reason not to mirror it, taken in talyvor-track on 2026-08-26:

```
git grep -licE 'rotat' -- internal/integrations   →  0 production files
git grep -licE 'encrypt' -- internal/integrations →  4 files   (positive control)
```

Track's credential store has **no rotation path at all**, and under a single key it could not have a
cheap one: rotating means decrypting and re-encrypting every stored secret. A credential store that
cannot rotate its own key is the same product gap W1.9.1 exists for, one layer down.

Envelope splits it in two. Each secret gets its own random **DEK**; the DEK is wrapped by the
operator's **KEK**. Rotation rewraps DEKs and **never touches the ciphertext of the secret itself** —
`Keyring.Rewrap` is pinned to that by `TestP5`, whose failure message says so, and control M4
(reimplementing `Rewrap` as a re-encrypt) turns it red.

What is mirrored from Track, verbatim, is the better half of the pointer: **the posture**.

## The posture

| `LENS_PROVIDER_SECRET_KEK` | Result |
|---|---|
| unset / empty | `ProviderSecretsEnabled()` is **false**. Custody is off. Lens boots normally. **There is no plaintext fallback.** |
| one or more valid base64 32-byte keys | Custody armed. The **first** key is primary; the rest are decrypt-only. |
| anything else — bad base64, wrong length, a repeated key | **Lens refuses to boot**, and the error names `LENS_PROVIDER_SECRET_KEK`. |

A wrong key is fail-loud at startup, never a broken-crypto surprise at the first customer
credential. Generate one with `openssl rand -base64 32`.

⚠ The variable is declared in `docker-compose.yaml`'s `environment:` list. It must therefore **not**
appear in `lens.env.example` — compose's `environment:` shadows `env_file`, and a variable in both
arrives empty. `TestNoVariableIsBothCuratedAndInLensEnvExample` enforces that.

## Rotating the KEK

Rotation is a rewrap, not a re-encrypt. It needs the keyring and nothing else — no plaintexts, no
per-row context — so it can run in one pass.

1. Generate the new key: `openssl rand -base64 32`.
2. **Prepend** it: `LENS_PROVIDER_SECRET_KEK=<new>,<old>`. New seals immediately land on the new
   key; everything sealed under the old key still opens.
3. Run a rewrap pass over the stored secrets (`Keyring.Rewrap` per row). Each row's `KeyID` moves to
   the new key's id.
4. When no stored row still names the old key id, drop it: `LENS_PROVIDER_SECRET_KEK=<new>`.

`Keyring.KeyIDs()` reports what the ring can open, primary first; a row's `KeyID` reports what it
needs. The difference between those two sets is exactly "how much of the rotation is left".

⚠ Do not skip step 3. Dropping the old key while rows still reference it strands them permanently —
see below.

## What happens if the KEK is lost

**Every secret wrapped under it is unrecoverable.** There is no escrow, no recovery code, and no
Talyvor-side copy. That is the property, not an oversight: it is what lets Talyvor say that a
compromise of the database alone does not hand over customer provider accounts. A KEK that Talyvor
could recover is a KEK an attacker with Talyvor's own infrastructure could recover too.

**What the operator sees** is deliberately specific. `Keyring.Use` returns `ErrUnknownKey`, and the
message names the key id the record needed and lists the ids the ring holds:

```
envelope: no key in the ring matches this record's key id: key id "3f9a1c02" (the ring holds [b71e04d5])
```

That is a different error from a tampered or corrupt record, which reports an authentication
failure. The distinction is pinned by `TestP8` and control M8: without it, an operator who dropped a
KEK too early is told "decrypt failed" and goes looking for data corruption that is not there.

**Recovery is re-entry, not decryption.** The customer supplies the provider credential again. Any
BYOK UI built on this must be able to say "we cannot read your key, please paste it again" — which
is the same sentence as the security guarantee, and it should be worded as one.

## The contract W6.4 / W6.8 must honour

1. **Gate on `Config.ProviderSecretsEnabled()`.** Do not register a custody route, and do not accept
   a provider secret on any path, while it is false. There is no plaintext column to fall back to
   and there must never be one.
2. **Never return a plaintext.** `internal/envelope` exposes no function that hands one back;
   `Keyring.Use` lends it to a callback and wipes the buffer before returning. `TestP6` is a census
   over the package's exported surface, so a future getter goes red rather than unnoticed — keep the
   same rule at the API layer. `TestK5` does the config-layer half.
3. **Verify by USE, never by read-back.** To confirm a stored credential is right, present it
   upstream and see whether the provider accepts it. Do not add a "reveal" or "check" endpoint. A
   200 from anything that did not actually use the credential is the shape of Track's `lens_healthy`
   defect, which reported healthy precisely when the credential was missing.
4. **Pass a real `aad`.** `Seal`/`Use` bind a record to its context and the aad is **not stored** —
   the caller must supply the same bytes back. Use something that identifies the row's owner and
   slot, e.g. `workspaceID + "|" + provider`. Without it, copying one workspace's ciphertext into
   another's row lets the second workspace bill the first one's provider account (`TestP4`).
5. **Store `Sealed` whole.** Every field is ciphertext, a nonce or a key id. `KeyID` in particular
   must be persisted and queryable, or step 3 of the rotation runbook cannot be performed.

## Two things measured on the way, neither of them fixed here

**(a) The compose-forwarding guard cannot see a new variable.** `cmd/lens/compose_env_reach_test.go`
works from a hand-written `mustForward` map. Measured: with `LENS_PROVIDER_SECRET_KEK` absent from
`docker-compose.yaml` **entirely**, that test was **green**. It went red only after the variable was
added to the map by hand — the same hand that would have remembered the compose line. The test's own
header argues for a curated list over a 160-variable census and that argument still holds; what is
recorded is that its written inclusion rule is enforced by whoever remembers it. `TestEnvExample-
DocumentsEveryConfigVariable` in the same directory is the derived census, and it *did* catch this
variable unprompted — the two guards are not interchangeable.

**(b) Lens already accepts a provider secret over HTTP.** `POST /v1/api/keys/pool`
(`cmd/lens/main.go:4707`) takes `{provider, key, alias, rate_limit}` and calls `keypool.Pool.Add`.
It is `requireAdmin`-gated, the response echoes only `{id, provider, alias}` (never the key), and the
pool is **in-memory only** — a restart empties it. Measured rather than read: `internal/keypool`'s
direct imports are `context errors fmt github.com/google/uuid log/slog sync time`, and there is no
database client anywhere in its transitive set (`go list -deps` shows `database/sql/driver` only,
pulled in by `google/uuid` implementing `driver.Valuer`; positive control — `./internal/proxy` has
11 pgx packages in the same query).

So W6.10's precondition holds for *stored* provider secrets, which is what it is about, but the
sentence "nothing accepts a provider secret" is not literally true today and a reader should know
which of the two they are relying on.
