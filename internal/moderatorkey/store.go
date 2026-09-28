// Package moderatorkey is B20.13's marketplace moderator credential: a key with ONE scope, marketplace
// moderation, for the web app's review queue. It reads the review queue, approves a listing and takes one
// down, and nothing else — every other admin route answers 403 to it (cmd/lens/authz_admin_handlers.go).
//
// Keys are created and revoked by the operator command `lens moderator-keys`, stored as a sha256 hash
// (the raw key is shown once, at creation), and every use is recorded in moderator_key_uses with the
// operator the web app names. Migration 0155.
package moderatorkey

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
	// KeyPrefix marks a moderator key. Disjoint from the workspace (tlv_ws_) and session (tlv_sk_)
	// prefixes, so auth.Manager can route on it without a lookup.
	KeyPrefix = "tlv_mod_"

	keyRandBytes     = 24
	displayPrefixLen = len(KeyPrefix) + 8
)

// ErrInvalid covers an unknown, malformed and revoked key alike, so the wire never says which.
var ErrInvalid = errors.New("moderatorkey: invalid, unknown or revoked moderator key")

// Key is one moderator key, never its raw value.
type Key struct {
	ID        int64
	Name      string
	KeyPrefix string
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
	RevokedBy string
}

// Use is one recorded use of a moderator key.
type Use struct {
	KeyID     int64
	Operator  string
	Method    string
	Path      string
	CreatedAt time.Time
}

// Store reads and writes moderator_keys and moderator_key_uses.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a Store over pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Create makes a new key named name, created by by, and returns its raw value — the only time it exists
// outside the caller's hands.
func (s *Store) Create(ctx context.Context, name, by string) (string, *Key, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil, errors.New("moderatorkey: a name is required")
	}
	if s.pool == nil {
		return "", nil, errors.New("moderatorkey: no database configured")
	}
	buf := make([]byte, keyRandBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("moderatorkey: read random: %w", err)
	}
	raw := KeyPrefix + hex.EncodeToString(buf)
	k := &Key{Name: name, KeyPrefix: raw[:displayPrefixLen], CreatedBy: by}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO moderator_keys (name, key_hash, key_prefix, created_by) VALUES ($1, $2, $3, $4)
		 RETURNING id, created_at`,
		k.Name, hash(raw), k.KeyPrefix, k.CreatedBy).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("moderatorkey: insert: %w", err)
	}
	return raw, k, nil
}

// Validate returns the key raw belongs to, or ErrInvalid when it is malformed, unknown or revoked. It
// reads the database every time, so a revocation takes effect on the next request.
func (s *Store) Validate(ctx context.Context, raw string) (*Key, error) {
	if !strings.HasPrefix(raw, KeyPrefix) || len(raw) != len(KeyPrefix)+keyRandBytes*2 || s.pool == nil {
		return nil, ErrInvalid
	}
	var k Key
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, key_prefix, created_by, created_at, revoked_at, revoked_by
		   FROM moderator_keys WHERE key_hash = $1`, hash(raw)).
		Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &k.RevokedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("moderatorkey: lookup: %w", err)
	}
	if k.RevokedAt != nil {
		return nil, ErrInvalid
	}
	return &k, nil
}

// Revoke stops key id from working. Revoking an already-revoked key keeps its first revocation.
func (s *Store) Revoke(ctx context.Context, id int64, by string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE moderator_keys SET revoked_at = now(), revoked_by = $2 WHERE id = $1 AND revoked_at IS NULL`, id, by)
	if err != nil {
		return fmt.Errorf("moderatorkey: revoke: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM moderator_keys WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("moderatorkey: revoke: %w", err)
		}
		if !exists {
			return fmt.Errorf("moderatorkey: no key %d", id)
		}
	}
	return nil
}

// List returns every key, newest first.
func (s *Store) List(ctx context.Context) ([]Key, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, key_prefix, created_by, created_at, revoked_at, revoked_by
		   FROM moderator_keys ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("moderatorkey: list: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &k.CreatedBy, &k.CreatedAt, &k.RevokedAt, &k.RevokedBy); err != nil {
			return nil, fmt.Errorf("moderatorkey: list: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordUse appends one use of key keyID to the audit log.
func (s *Store) RecordUse(ctx context.Context, keyID int64, operator, method, path string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO moderator_key_uses (key_id, operator, method, path) VALUES ($1, $2, $3, $4)`,
		keyID, operator, method, path)
	if err != nil {
		return fmt.Errorf("moderatorkey: record use: %w", err)
	}
	return nil
}

// Uses returns key keyID's recorded uses, newest first, at most limit.
func (s *Store) Uses(ctx context.Context, keyID int64, limit int) ([]Use, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key_id, operator, method, path, created_at FROM moderator_key_uses
		  WHERE key_id = $1 ORDER BY id DESC LIMIT $2`, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("moderatorkey: uses: %w", err)
	}
	defer rows.Close()
	var out []Use
	for rows.Next() {
		var u Use
		if err := rows.Scan(&u.KeyID, &u.Operator, &u.Method, &u.Path, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("moderatorkey: uses: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
