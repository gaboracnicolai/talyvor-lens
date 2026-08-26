package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Two-phase key rotation — W1.9.1: a rotation a RUNNING SERVICE can survive.
//
// ── WHY RotateAPIKey IS NOT THIS, AND IS NOT WRONG EITHER ────────────────────────────────────
//
// RotateAPIKey wraps SELECT FOR UPDATE → INSERT new → DELETE old in ONE transaction. Its ordering
// comment is true about the transaction and about the WORKSPACE: no instant exists in which the
// workspace has zero keys. The HOLDER is a different party. Both rows change visibility at COMMIT,
// atomically, so there is no instant at which a client can authenticate with the old key AND the
// new one — and a service still presenting the old credential is locked out for the whole of
// "write the new value into config → restart the process". internal/tenant's
// rotation_overlap_integration_test.go pins that against real Postgres.
//
// That makes RotateAPIKey the RIGHT primitive for a key held by a person who can paste the new
// value immediately, or for a leaked key that must die now and downtime is the lesser harm. It is
// the WRONG one for a key a process holds, and that is a property of the caller rather than a bug
// in the function. Both paths therefore ship; this file is the second one.
//
// ── THE ORDER IS THE DESIGN AND IT MUST NOT INVERT ───────────────────────────────────────────
//
//	BeginRotation      mint the new key; the old one KEEPS WORKING
//	(the holder switches, at its own pace)
//	CompleteRotation   revoke the old key — REFUSED until the new key has authenticated
//
// ⚠ THAT REFUSAL IS THE PART TWO RAW CALLS CANNOT GIVE YOU. CreateAPIKey + RevokeAPIKey already
// produce an overlap window, so the primitives existed; what did not exist was any way to stop an
// operator revoking before the holder had actually switched. W1.9.1's instruction is "VERIFY WITH
// AN AUTHENTICATED REQUEST, never a 200 from an unauthenticated health route — that is the exact
// shape of Track's lens_healthy defect, which reported healthy precisely when the credential was
// missing." As an instruction that is a habit; here it is a precondition the store enforces, and
// the evidence is the one thing that cannot be faked by a probe: workspace_api_keys.last_used_at,
// which ValidateAPIKey touches only on a successful bcrypt match.
//
// ⚠ AND IT IS COMPARED AGAINST started_at, NOT AGAINST NULL. "Has this key ever been used" would
// pass for any pre-existing key an operator pointed a rotation at; "has it been used SINCE this
// rotation opened" is the question that means the holder switched.

var (
	// ErrRotationOpen — one open rotation per key. Two would mint two replacements and leave the
	// operator unable to say which one the holder took.
	ErrRotationOpen = errors.New("tenant: a rotation is already open for this key")
	// ErrRotationNotFound — no such rotation for this workspace. Also what a caller from another
	// workspace gets, because a rotation revokes a key and must not be reachable across tenants.
	ErrRotationNotFound = errors.New("tenant: rotation not found")
	// ErrRotationClosed — already completed or abandoned.
	ErrRotationClosed = errors.New("tenant: rotation is already closed")
	// ErrNewKeyUnused — the new key has not authenticated a request since the rotation opened, so
	// revoking the old one now would take the holder offline. This is the guard, not an obstacle.
	ErrNewKeyUnused = errors.New("tenant: the new key has not authenticated a request since the rotation opened")
)

// Rotation is one rotation in flight (or its history).
type Rotation struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	OldKeyID    string     `json:"old_key_id"`
	NewKeyID    string     `json:"new_key_id"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
}

// RotationStatus answers the only question an operator has mid-rotation: can I finish yet?
type RotationStatus struct {
	Rotation
	// NewKeyUsed is true once the new key has authenticated a request SINCE StartedAt. It is the
	// completion precondition, reported so the operator can watch for it rather than guess.
	NewKeyUsed bool `json:"new_key_used"`
	// NewKeyLastUsedAt is nil until the holder switches.
	NewKeyLastUsedAt *time.Time `json:"new_key_last_used_at,omitempty"`
	// NextStep is what to do now, in words, because an operator reading this at 3am should not
	// have to infer it from two booleans.
	NextStep string `json:"next_step"`
}

const (
	selectOpenRotationSQL = `
SELECT id FROM workspace_key_rotations
WHERE old_key_id = $1 AND completed_at IS NULL`

	insertRotationSQL = `
INSERT INTO workspace_key_rotations (workspace_id, old_key_id, new_key_id)
VALUES ($1, $2, $3)
RETURNING id, started_at`

	selectRotationForUpdateSQL = `
SELECT id, workspace_id, old_key_id, new_key_id, started_at, completed_at, COALESCE(outcome, '')
FROM workspace_key_rotations
WHERE id = $1 AND workspace_id = $2
FOR UPDATE`

	selectRotationSQL = `
SELECT id, workspace_id, old_key_id, new_key_id, started_at, completed_at, COALESCE(outcome, '')
FROM workspace_key_rotations
WHERE id = $1 AND workspace_id = $2`

	selectKeyLastUsedSQL = `SELECT last_used_at FROM workspace_api_keys WHERE id = $1`

	closeRotationSQL = `
UPDATE workspace_key_rotations SET completed_at = NOW(), outcome = $2 WHERE id = $1`
)

// BeginRotation mints a replacement for keyID and records the pairing. The OLD KEY IS NOT TOUCHED:
// both credentials authenticate until CompleteRotation is called and allowed.
//
// The old key row is locked FOR UPDATE, which serialises concurrent begins on the same key, so the
// open-rotation check below is exact. The partial unique index on (old_key_id) WHERE completed_at
// IS NULL is the backstop rather than the mechanism.
func (s *Store) BeginRotation(ctx context.Context, workspaceID, keyID string) (string, *WorkspaceAPIKey, *Rotation, error) {
	if s.pool == nil {
		return "", nil, nil, errors.New("tenant: no database")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, nil, fmt.Errorf("tenant: begin rotation tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var old WorkspaceAPIKey
	if err := tx.QueryRow(ctx, selectKeyForRotateSQL, keyID, workspaceID).Scan(
		&old.ID, &old.WorkspaceID, &old.KeyPrefix, &old.Name, &old.Scopes, &old.ExpiresAt, &old.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, nil, ErrKeyNotFound
		}
		return "", nil, nil, fmt.Errorf("tenant: lock key for rotation: %w", err)
	}

	var openID string
	switch err := tx.QueryRow(ctx, selectOpenRotationSQL, keyID).Scan(&openID); {
	case err == nil:
		return "", nil, nil, ErrRotationOpen
	case errors.Is(err, pgx.ErrNoRows):
		// the expected path
	default:
		return "", nil, nil, fmt.Errorf("tenant: check open rotation: %w", err)
	}

	raw, prefix, err := GenerateKey()
	if err != nil {
		return "", nil, nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(raw), BcryptCost)
	if err != nil {
		return "", nil, nil, fmt.Errorf("tenant: bcrypt: %w", err)
	}
	// The replacement inherits name, scopes and expiry, exactly as RotateAPIKey's does: a rotation
	// that silently widened or narrowed a key's scopes would be a privilege change wearing the
	// clothes of a credential change.
	fresh := &WorkspaceAPIKey{
		WorkspaceID: workspaceID,
		KeyHash:     string(hash),
		KeyPrefix:   prefix,
		Name:        old.Name,
		Scopes:      append([]string{}, old.Scopes...),
		ExpiresAt:   old.ExpiresAt,
	}
	if err := tx.QueryRow(ctx, insertKeySQL,
		workspaceID, fresh.KeyHash, fresh.KeyPrefix, fresh.Name, fresh.Scopes, fresh.ExpiresAt,
	).Scan(&fresh.ID, &fresh.CreatedAt); err != nil {
		return "", nil, nil, fmt.Errorf("tenant: insert rotation key: %w", err)
	}

	rot := &Rotation{WorkspaceID: workspaceID, OldKeyID: old.ID, NewKeyID: fresh.ID}
	if err := tx.QueryRow(ctx, insertRotationSQL, workspaceID, old.ID, fresh.ID).
		Scan(&rot.ID, &rot.StartedAt); err != nil {
		return "", nil, nil, fmt.Errorf("tenant: record rotation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", nil, nil, fmt.Errorf("tenant: commit rotation: %w", err)
	}
	return raw, fresh, rot, nil
}

// RotationStatus reports whether the rotation can be completed yet, and what to do next.
func (s *Store) RotationStatus(ctx context.Context, workspaceID, rotationID string) (*RotationStatus, error) {
	if s.pool == nil {
		return nil, errors.New("tenant: no database")
	}
	var rot Rotation
	if err := s.pool.QueryRow(ctx, selectRotationSQL, rotationID, workspaceID).Scan(
		&rot.ID, &rot.WorkspaceID, &rot.OldKeyID, &rot.NewKeyID, &rot.StartedAt, &rot.CompletedAt, &rot.Outcome,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRotationNotFound
		}
		return nil, fmt.Errorf("tenant: read rotation: %w", err)
	}

	var lastUsed *time.Time
	if err := s.pool.QueryRow(ctx, selectKeyLastUsedSQL, rot.NewKeyID).Scan(&lastUsed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("tenant: read new key usage: %w", err)
	}

	st := &RotationStatus{Rotation: rot, NewKeyLastUsedAt: lastUsed}
	st.NewKeyUsed = usedSince(lastUsed, rot.StartedAt)
	switch {
	case rot.CompletedAt != nil:
		st.NextStep = "nothing — this rotation is " + rot.Outcome
	case st.NewKeyUsed:
		st.NextStep = "complete the rotation; the new key has authenticated a request, so revoking the old one will not take the holder offline"
	default:
		st.NextStep = "put the new key in front of the holder and let it make one real request. Completion is refused until the new key authenticates — a health check does not count, because a health route answers 200 with no credential at all"
	}
	return st, nil
}

// CompleteRotation revokes the old key — and only if the new one has been used since the rotation
// opened. A refusal changes nothing: both keys are still live afterwards, because a guard that
// errors AND revokes has already caused the outage it exists to prevent.
func (s *Store) CompleteRotation(ctx context.Context, workspaceID, rotationID string) error {
	return s.closeRotation(ctx, workspaceID, rotationID, "completed", func(ctx context.Context, tx pgx.Tx, rot *Rotation) error {
		var lastUsed *time.Time
		if err := tx.QueryRow(ctx, selectKeyLastUsedSQL, rot.NewKeyID).Scan(&lastUsed); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("tenant: the rotation's new key no longer exists")
			}
			return fmt.Errorf("tenant: read new key usage: %w", err)
		}
		if !usedSince(lastUsed, rot.StartedAt) {
			return ErrNewKeyUnused
		}
		if _, err := tx.Exec(ctx, revokeKeySQL, rot.OldKeyID); err != nil {
			return fmt.Errorf("tenant: revoke old key: %w", err)
		}
		return nil
	})
}

// AbandonRotation calls it off: the NEW key is destroyed and the holder's key keeps working.
//
// ⚠ WHICH KEY DIES HERE IS THE WHOLE POINT. An operator reaches for abandon when something has
// already gone wrong — the holder cannot take the new value, a deploy failed halfway. Killing the
// old key here would be the outage arrived at from the direction of someone already in trouble.
func (s *Store) AbandonRotation(ctx context.Context, workspaceID, rotationID string) error {
	return s.closeRotation(ctx, workspaceID, rotationID, "abandoned", func(ctx context.Context, tx pgx.Tx, rot *Rotation) error {
		if _, err := tx.Exec(ctx, revokeKeySQL, rot.NewKeyID); err != nil {
			return fmt.Errorf("tenant: revoke abandoned key: %w", err)
		}
		return nil
	})
}

// closeRotation is the shared shape: lock the row, refuse a closed one, run the caller's action,
// stamp the outcome. The action runs INSIDE the transaction, so a refusal rolls back whatever it
// had started.
func (s *Store) closeRotation(
	ctx context.Context, workspaceID, rotationID, outcome string,
	act func(context.Context, pgx.Tx, *Rotation) error,
) error {
	if s.pool == nil {
		return errors.New("tenant: no database")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenant: begin close tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rot Rotation
	if err := tx.QueryRow(ctx, selectRotationForUpdateSQL, rotationID, workspaceID).Scan(
		&rot.ID, &rot.WorkspaceID, &rot.OldKeyID, &rot.NewKeyID, &rot.StartedAt, &rot.CompletedAt, &rot.Outcome,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRotationNotFound
		}
		return fmt.Errorf("tenant: lock rotation: %w", err)
	}
	if rot.CompletedAt != nil {
		return ErrRotationClosed
	}
	if err := act(ctx, tx, &rot); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, closeRotationSQL, rot.ID, outcome); err != nil {
		return fmt.Errorf("tenant: stamp rotation outcome: %w", err)
	}
	return tx.Commit(ctx)
}

// usedSince is the completion predicate, in one place so the status report and the gate cannot
// drift. NULL is not proof, and neither is a timestamp from before the rotation opened.
func usedSince(lastUsed *time.Time, startedAt time.Time) bool {
	return lastUsed != nil && lastUsed.After(startedAt)
}
