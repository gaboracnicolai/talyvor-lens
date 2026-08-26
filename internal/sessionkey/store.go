package sessionkey

// store.go — W4.6.1 STEP 4: the session-scoped credential, so a browser chat never holds a
// workspace key.
//
// ⚠ WHAT THIS PACKAGE DELIBERATELY DOES NOT HAVE: A SCOPE COLUMN.
//
// A scope column is a scope a ROW can carry, and a scope a row can carry is one a caller can
// eventually be persuaded to set. That is not hypothetical here — it is the defect measured on the
// neighbouring credential and written up in docs/model2-session-credentials.md: the workspace-key
// mint route takes `scopes` from the request body and has no gate, so a credential can mint a
// credential carrying scopes it does not itself hold. A session key cannot repeat that shape
// because there is nowhere to put a scope. What it authorises is a CONSTANT in internal/auth, next
// to the gate that reads it.
//
// ⚠ AND IT HAS NO `revoked` COLUMN EITHER — REVOCATION IS A DELETE.
//
// A boolean means every read path must remember to filter on it, and the one that forgets is a
// revoked credential that still works. A deleted row cannot be forgotten by a query.
//
// ── WHY sha256 AND NOT bcrypt, WHEN internal/tenant USES bcrypt ────────────────────────────────
//
// Both are in this repo already: internal/tenant hashes workspace keys with bcrypt cost 10 behind a
// prefix-indexed scan; internal/auth's KeyStore hashes api_keys with sha256 and looks them up by
// hash. Session keys follow the SECOND, for three reasons that are properties of this credential
// rather than preferences:
//
//   1. bcrypt's work factor exists to make a DICTIONARY attack expensive — it protects secrets a
//      human chose. A session key is 24 bytes from crypto/rand; there is no dictionary. The work
//      factor buys nothing here and costs ~100ms.
//   2. That ~100ms would be paid on EVERY chat request, on the streaming path, where it is the
//      difference between a responsive product and a slow one. Auth is not the place to spend a
//      budget that buys nothing.
//   3. A hash lookup is a single indexed equality on a UNIQUE column, so it needs no cache — and
//      NOT CACHING is what makes revocation immediate. internal/tenant records why that property
//      is worth protecting: a cached credential is one that keeps working after it is revoked.
//
// ⚠ THE TIMING QUESTION, ANSWERED RATHER THAN WAVED AT: an attacker cannot walk a timing signal
// toward a 192-bit preimage, so a constant-time compare would be ceremony. The secret never
// reaches a comparison in this package at all — only its hash reaches SQL.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// KeyPrefix is what Manager.Authenticate dispatches on. It must not collide with
	// tenant.KeyPrefix ("tlv_ws_") or auth's api_keys prefix, or one credential shape would be
	// looked up in the wrong store — TestPrefixesAreDisjoint pins that.
	KeyPrefix = "tlv_sk_"

	// keyRandBytes is the entropy behind the prefix. 24 bytes = 192 bits.
	keyRandBytes = 24

	// displayPrefixLen is how much of the key is kept in the clear for an operator to identify a
	// row by. It is NOT a lookup key — lookup is by hash — so it carries no requirement to be
	// unique and leaks nothing useful at this length.
	displayPrefixLen = len(KeyPrefix) + 8
)

var (
	ErrInvalid = errors.New("sessionkey: invalid or unknown session key")
	ErrExpired = errors.New("sessionkey: session key has expired")
)

// SessionKey is the persisted view. There is no plaintext field: the raw key is returned once from
// Mint and never stored.
type SessionKey struct {
	ID          string
	WorkspaceID string
	UserID      string
	KeyPrefix   string
	ExpiresAt   time.Time
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

const insertSQL = `
INSERT INTO session_keys (workspace_id, user_id, key_hash, key_prefix, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, created_at`

// Mint issues a session key for (workspaceID, userID) that expires ttl from now.
//
// ⚠ ttl IS NOT CLAMPED HERE, AND THAT IS ON PURPOSE. The ceiling a session key must respect is the
// remaining life of the SESSION that asked for it, and this package cannot see that. Clamping
// happens at the HTTP boundary where the caller's own expiry is known
// (cmd/lens/session_key_handler.go). A clamp in two places is a clamp nobody can state the value
// of. A NEGATIVE or zero ttl is accepted and produces an already-expired row — the tests use it to
// reach the expired branch without sleeping, and an already-dead credential is harmless.
func (s *Store) Mint(ctx context.Context, workspaceID, userID string, ttl time.Duration) (string, *SessionKey, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return "", nil, errors.New("sessionkey: workspace id required")
	}
	if strings.TrimSpace(userID) == "" {
		return "", nil, errors.New("sessionkey: user id required")
	}
	if s.pool == nil {
		return "", nil, errors.New("sessionkey: no database configured")
	}
	buf := make([]byte, keyRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("sessionkey: read random: %w", err)
	}
	raw := KeyPrefix + hex.EncodeToString(buf)

	k := &SessionKey{
		WorkspaceID: workspaceID,
		UserID:      userID,
		KeyPrefix:   raw[:displayPrefixLen],
		ExpiresAt:   time.Now().Add(ttl).UTC(),
	}
	row := s.pool.QueryRow(ctx, insertSQL, k.WorkspaceID, k.UserID, hash(raw), k.KeyPrefix, k.ExpiresAt)
	if err := row.Scan(&k.ID, &k.CreatedAt); err != nil {
		return "", nil, fmt.Errorf("sessionkey: insert: %w", err)
	}
	return raw, k, nil
}

const selectByHashSQL = `
SELECT id, workspace_id, user_id, key_prefix, expires_at, created_at, last_used_at
FROM session_keys
WHERE key_hash = $1`

const touchSQL = `UPDATE session_keys SET last_used_at = NOW() WHERE id = $1`

// Validate resolves a raw session key to its owner.
//
// ⚠ NEVER CACHED. Every call is a query, so RevokeAll takes effect on the very next request rather
// than "within five minutes" — see the package comment.
func (s *Store) Validate(ctx context.Context, raw string) (*SessionKey, error) {
	// The prefix check is a cheap reject, not a security boundary: it stops a workspace key being
	// looked up in this table (and vice versa), which would otherwise be a silent cross-store miss
	// rather than a clear refusal.
	if !strings.HasPrefix(raw, KeyPrefix) || len(raw) != len(KeyPrefix)+keyRandBytes*2 {
		return nil, ErrInvalid
	}
	if s.pool == nil {
		return nil, ErrInvalid
	}
	var k SessionKey
	err := s.pool.QueryRow(ctx, selectByHashSQL, hash(raw)).Scan(
		&k.ID, &k.WorkspaceID, &k.UserID, &k.KeyPrefix, &k.ExpiresAt, &k.CreatedAt, &k.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("sessionkey: lookup: %w", err)
	}
	if !k.ExpiresAt.After(time.Now()) {
		return nil, ErrExpired
	}
	// Best-effort; a failure to record last-used must never refuse a valid credential.
	_, _ = s.pool.Exec(ctx, touchSQL, k.ID)
	now := time.Now().UTC()
	k.LastUsedAt = &now
	return &k, nil
}

const revokeAllSQL = `DELETE FROM session_keys WHERE workspace_id = $1 AND user_id = $2`

// RevokeAll is what sign-out calls. It returns how many keys it removed so the caller can log a
// number rather than an assumption.
//
// ⚠ SCOPED TO (workspace, user), NOT TO workspace. Revoking a colleague's live chat because someone
// else in the same workspace signed out is a bug that looks like a security feature.
func (s *Store) RevokeAll(ctx context.Context, workspaceID, userID string) (int64, error) {
	if s.pool == nil {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, revokeAllSQL, workspaceID, userID)
	if err != nil {
		return 0, fmt.Errorf("sessionkey: revoke all: %w", err)
	}
	return tag.RowsAffected(), nil
}

const revokeSQL = `DELETE FROM session_keys WHERE id = $1 AND workspace_id = $2`

// Revoke removes one key.
//
// ⚠ workspaceID IS IN THE WHERE CLAUSE, NOT CHECKED BY THE CALLER. Revoking by id alone would let
// anyone who learns an id kill another tenant's live session; a caller-side ownership check is one
// a future caller forgets. Idempotent: naming a foreign or absent key is a silent no-op, which is
// also what stops the route being an existence oracle for other tenants' key ids.
func (s *Store) Revoke(ctx context.Context, workspaceID, id string) error {
	if s.pool == nil {
		return nil
	}
	if _, err := s.pool.Exec(ctx, revokeSQL, id, workspaceID); err != nil {
		return fmt.Errorf("sessionkey: revoke: %w", err)
	}
	return nil
}
