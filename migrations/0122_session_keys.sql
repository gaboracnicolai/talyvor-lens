-- 0122_session_keys.sql — W4.6.1 step 4: SESSION-SCOPED KEYS.
--
-- ── WHY A SEPARATE TABLE AND NOT A FLAG ON workspace_api_keys ─────────────────
--
-- The obvious cheap move is a `kind` column on workspace_api_keys. It is wrong in
-- four specific ways, each of which is a real behaviour rather than a taste:
--
--   1. ListAPIKeys is what the Keys screen renders. A chat session key is not a
--      credential a user manages, and putting it there means every open tab shows
--      up as a key the user is invited to reason about.
--   2. RotateAPIKey inherits name, scopes and expiry onto a fresh key. Rotating a
--      session credential is meaningless, and a meaningless operation reachable by
--      id is one somebody eventually calls.
--   3. AuthContext.APIKeyID keys the F4 per-agent LXC sub-budget allocator. A
--      credential that dies in an hour must not open a sub-budget.
--   4. "Revoke everything this user's browser holds" must not be able to reach the
--      user's real workspace keys. Two tables make that impossible; one table makes
--      it a WHERE clause somebody has to get right, every time, forever.
--
-- ── WHAT THE COLUMNS ENFORCE ─────────────────────────────────────────────────
--
-- ⚠ expires_at IS NOT NULL. "A session key always expires" is the property that
-- makes it a SESSION key rather than a workspace key wearing a different name, and
-- a property enforced by the column cannot be forgotten by a caller, a migration,
-- or a future route. NULL would mean "forever", and forever is the thing this
-- credential exists to not be.
--
-- ⚠ THERE IS NO scopes COLUMN, AND THAT IS THE POINT. A scope column is a scope a
-- row can carry, and a scope a row can carry is one a caller can eventually be
-- persuaded to set. docs/model2-session-credentials.md measures exactly that on the
-- neighbouring credential: workspace_api_keys.scopes comes from the request body,
-- through a route with no scope gate, so a credential can mint a credential
-- carrying scopes it does not itself hold. What a session key authorises is a
-- constant in internal/auth next to the gate that reads it, asserted by VALUE in
-- internal/auth/session_key_auth_test.go.
--
-- ⚠ THERE IS NO revoked COLUMN EITHER — revocation is a DELETE. A boolean means
-- every read path must remember to filter on it, and the one path that forgets is a
-- revoked credential that still works. A deleted row cannot be forgotten by a query.
--
-- ⚠ key_hash IS sha256, NOT bcrypt, AND UNIQUE. Both hashes are already in this
-- repo (internal/tenant bcrypts workspace keys; internal/auth sha256s api_keys).
-- The work factor in bcrypt buys resistance to a DICTIONARY attack on a
-- human-chosen secret; a session key is 24 bytes from crypto/rand and has no
-- dictionary. What the work factor WOULD cost is ~100ms on every chat request. The
-- UNIQUE index makes validation one indexed equality, which is what lets the
-- validator hold no cache — and holding no cache is what makes revocation immediate
-- rather than "within five minutes".
--
-- ⚠ user_id IS PART OF THE IDENTITY, NOT DECORATION. Sign-out is per USER. Revoking
-- a colleague's live chat because someone else in the same workspace signed out is a
-- bug that looks like a security feature.

CREATE TABLE IF NOT EXISTS session_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    key_hash     TEXT NOT NULL,
    key_prefix   TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

-- Validation is a single equality on this index. UNIQUE because two rows sharing a
-- hash would mean either a duplicate credential or a collision, and both should
-- fail loudly at INSERT rather than resolve arbitrarily at SELECT.
CREATE UNIQUE INDEX IF NOT EXISTS session_keys_hash_idx ON session_keys (key_hash);

-- Sign-out's DELETE, and the per-owner listing behind it.
CREATE INDEX IF NOT EXISTS session_keys_owner_idx ON session_keys (workspace_id, user_id);

-- Expiry sweeps. Rows are not self-cleaning: an expired key is REFUSED by the
-- validator (so it is harmless), but it is still a row, and without this index a
-- housekeeping delete is a sequential scan of every session ever minted.
CREATE INDEX IF NOT EXISTS session_keys_expires_idx ON session_keys (expires_at);
