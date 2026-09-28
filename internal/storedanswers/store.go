// Package storedanswers is what a workspace has stored in Lens's answer stores, and deleting it
// (B21.3, decided by Nicolai 28 Sep 2026).
//
// Switching sharing off stops NEW sharing only: answers shared before it stay shared, and keep
// earning their contributor, until they are deleted. Deletion is the workspace's choice, two ways:
// itself, through DELETE /v1/workspaces/{ws}/stored-answers, or by asking Talyvor, through a
// deletion request an operator completes. Both run Delete below and both leave a row in
// stored_answer_deletions saying who deleted what.
//
// WHAT IS DELETED, AND WHERE IT LIVES:
//   - answers: prompt_embeddings rows. Shared = is_poolable rows this workspace contributed (variant
//     rows carry the contributor too); private = its own is_poolable=false rows.
//   - conversions: Redis lens:distill:* keys. A shared conversion carries a parallel ":owner" key, a
//     private one a parallel ":ws" key (cache.DistillCache.SetPrivate); both name the workspace.
//   - cached copies: Redis lens:exact:* answers, each with a parallel ":owner" key naming the
//     workspace. Redis cannot tell a private copy from a shared one (both keys are hashes), so every
//     copy is removed on either scope. A private answer still serves afterwards from its stored row.
//
// WHAT IS NOT: billing and ledger records (lxc_ledger, lens_token_ledger, pool_royalty_mints), which
// the law requires to be kept, and earnings already final, which are not clawed back. An answer
// already given to another user stays in that user's conversation.
package storedanswers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Scope is what a deletion covers.
type Scope string

const (
	// ScopeShared deletes every answer and conversion this workspace shared.
	ScopeShared Scope = "shared"
	// ScopeAll also deletes its private answers and conversions.
	ScopeAll Scope = "all"
)

// Valid reports whether s is one of the two scopes.
func (s Scope) Valid() bool { return s == ScopeShared || s == ScopeAll }

// Counts is what a workspace has stored, or what a deletion removed.
type Counts struct {
	SharedAnswers      int64 `json:"shared_answers"`
	PrivateAnswers     int64 `json:"private_answers"`
	SharedConversions  int64 `json:"shared_conversions"`
	PrivateConversions int64 `json:"private_conversions"`
	CachedCopies       int64 `json:"cached_copies"`
}

// Request is one "delete everything" request.
type Request struct {
	ID          int64      `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	RequestedBy string     `json:"requested_by"`
	Note        string     `json:"note"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CompletedBy string     `json:"completed_by,omitempty"`
}

const (
	StatusRequested = "requested"
	StatusDone      = "done"
)

// ErrNotFound is a request id that does not exist.
var ErrNotFound = errors.New("deletion request not found")

// ErrAlreadyDone is a request an operator has already completed.
var ErrAlreadyDone = errors.New("deletion request already done")

// KeptRecords is what a completed deletion keeps, and why — said on every completion.
const KeptRecords = "Billing and ledger records (charges, credits, earnings) are kept, because the law requires them. " +
	"Answers already given to other users stay in their conversations."

// Store reads and deletes a workspace's stored answers.
type Store struct {
	db  *pgxpool.Pool
	rdb *redis.Client
}

// New builds a Store. rdb may be nil (no Redis copies to find or delete).
func New(db *pgxpool.Pool, rdb *redis.Client) *Store { return &Store{db: db, rdb: rdb} }

const countAnswersSQL = `SELECT
  count(*) FILTER (WHERE is_poolable AND contributor_workspace_id = $1),
  count(*) FILTER (WHERE NOT is_poolable AND workspace_id = $1)
FROM prompt_embeddings
WHERE contributor_workspace_id = $1 OR workspace_id = $1`

const deleteSharedSQL = `DELETE FROM prompt_embeddings WHERE is_poolable AND contributor_workspace_id = $1`

const deletePrivateSQL = `DELETE FROM prompt_embeddings WHERE NOT is_poolable AND workspace_id = $1`

const auditSQL = `INSERT INTO stored_answer_deletions
  (workspace_id, scope, deleted_by, deletion_request_id, shared_answers, private_answers,
   shared_conversions, private_conversions, cached_copies)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// Redis marker patterns: the value of each marker key is the workspace id; the value key is the
// marker key without its suffix.
const (
	sharedConversionMarkers  = "lens:distill:*:owner"
	privateConversionMarkers = "lens:distill:*:ws"
	cachedCopyMarkers        = "lens:exact:*:owner"
)

// Counts returns what wsID has stored.
func (s *Store) Counts(ctx context.Context, wsID string) (Counts, error) {
	var c Counts
	if err := s.db.QueryRow(ctx, countAnswersSQL, wsID).Scan(&c.SharedAnswers, &c.PrivateAnswers); err != nil {
		return Counts{}, fmt.Errorf("storedanswers: count answers: %w", err)
	}
	var err error
	if c.SharedConversions, err = s.redisMarked(ctx, sharedConversionMarkers, wsID, false); err != nil {
		return Counts{}, err
	}
	if c.PrivateConversions, err = s.redisMarked(ctx, privateConversionMarkers, wsID, false); err != nil {
		return Counts{}, err
	}
	if c.CachedCopies, err = s.redisMarked(ctx, cachedCopyMarkers, wsID, false); err != nil {
		return Counts{}, err
	}
	return c, nil
}

// Delete removes wsID's stored answers in scope, records who did it (by) and what it removed in
// stored_answer_deletions, and returns that. requestID is the deletion request this completes, or 0.
// Irreversible.
func (s *Store) Delete(ctx context.Context, wsID string, scope Scope, by string, requestID int64) (Counts, error) {
	if !scope.Valid() {
		return Counts{}, fmt.Errorf("storedanswers: scope must be %q or %q", ScopeShared, ScopeAll)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Counts{}, fmt.Errorf("storedanswers: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var c Counts
	tag, err := tx.Exec(ctx, deleteSharedSQL, wsID)
	if err != nil {
		return Counts{}, fmt.Errorf("storedanswers: delete shared answers: %w", err)
	}
	c.SharedAnswers = tag.RowsAffected()
	if scope == ScopeAll {
		if tag, err = tx.Exec(ctx, deletePrivateSQL, wsID); err != nil {
			return Counts{}, fmt.Errorf("storedanswers: delete private answers: %w", err)
		}
		c.PrivateAnswers = tag.RowsAffected()
	}

	// Redis has no transaction with Postgres. It goes before the commit, so a Redis failure leaves the
	// rows in place and the caller can simply run the deletion again.
	if c.SharedConversions, err = s.redisMarked(ctx, sharedConversionMarkers, wsID, true); err != nil {
		return Counts{}, err
	}
	if scope == ScopeAll {
		if c.PrivateConversions, err = s.redisMarked(ctx, privateConversionMarkers, wsID, true); err != nil {
			return Counts{}, err
		}
	}
	if c.CachedCopies, err = s.redisMarked(ctx, cachedCopyMarkers, wsID, true); err != nil {
		return Counts{}, err
	}

	var reqID *int64
	if requestID > 0 {
		reqID = &requestID
	}
	if _, err := tx.Exec(ctx, auditSQL, wsID, string(scope), by, reqID, c.SharedAnswers, c.PrivateAnswers,
		c.SharedConversions, c.PrivateConversions, c.CachedCopies); err != nil {
		return Counts{}, fmt.Errorf("storedanswers: audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Counts{}, fmt.Errorf("storedanswers: commit: %w", err)
	}
	return c, nil
}

// redisMarked counts — and with del, deletes — every marker key matching pattern whose value is wsID,
// together with the value key it marks.
func (s *Store) redisMarked(ctx context.Context, pattern, wsID string, del bool) (int64, error) {
	if s.rdb == nil {
		return 0, nil
	}
	suffix := pattern[strings.LastIndex(pattern, ":"):]
	var n int64
	iter := s.rdb.Scan(ctx, 0, pattern, 1000).Iterator()
	var batch []string
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		vals, err := s.rdb.MGet(ctx, batch...).Result()
		if err != nil {
			return fmt.Errorf("storedanswers: read %s: %w", pattern, err)
		}
		var doomed []string
		for i, v := range vals {
			if owner, ok := v.(string); ok && owner == wsID {
				n++
				doomed = append(doomed, batch[i], strings.TrimSuffix(batch[i], suffix))
			}
		}
		if del && len(doomed) > 0 {
			if err := s.rdb.Del(ctx, doomed...).Err(); err != nil {
				return fmt.Errorf("storedanswers: delete %s: %w", pattern, err)
			}
		}
		batch = batch[:0]
		return nil
	}
	for iter.Next(ctx) {
		batch = append(batch, iter.Val())
		if len(batch) == 500 {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("storedanswers: scan %s: %w", pattern, err)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return n, nil
}

const requestCols = `id, workspace_id, requested_by, note, status, requested_at, completed_at, COALESCE(completed_by, '')`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.RequestedBy, &r.Note, &r.Status, &r.RequestedAt, &r.CompletedAt, &r.CompletedBy)
	return r, err
}

// FileRequest records that wsID asked Talyvor to delete everything it holds for them. A request
// still open is returned rather than duplicated.
func (s *Store) FileRequest(ctx context.Context, wsID, by, note string) (Request, error) {
	r, err := scanRequest(s.db.QueryRow(ctx, `SELECT `+requestCols+` FROM deletion_requests
		WHERE workspace_id = $1 AND status = 'requested' ORDER BY requested_at DESC LIMIT 1`, wsID))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Request{}, fmt.Errorf("storedanswers: read request: %w", err)
	}
	r, err = scanRequest(s.db.QueryRow(ctx, `INSERT INTO deletion_requests (workspace_id, requested_by, note)
		VALUES ($1, $2, $3) RETURNING `+requestCols, wsID, by, note))
	if err != nil {
		return Request{}, fmt.Errorf("storedanswers: file request: %w", err)
	}
	return r, nil
}

// Requests returns wsID's requests, newest first.
func (s *Store) Requests(ctx context.Context, wsID string) ([]Request, error) {
	return s.list(ctx, `SELECT `+requestCols+` FROM deletion_requests WHERE workspace_id = $1 ORDER BY requested_at DESC`, wsID)
}

// List returns every request (open only when openOnly), oldest first — the operator's queue.
func (s *Store) List(ctx context.Context, openOnly bool) ([]Request, error) {
	q := `SELECT ` + requestCols + ` FROM deletion_requests`
	if openOnly {
		q += ` WHERE status = 'requested'`
	}
	return s.list(ctx, q+` ORDER BY requested_at`)
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]Request, error) {
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storedanswers: list requests: %w", err)
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("storedanswers: list requests: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Complete carries out request id: deletes everything its workspace has stored (ScopeAll) and marks
// it done, by the operator named in by. Billing and ledger records are kept (KeptRecords).
func (s *Store) Complete(ctx context.Context, id int64, by string) (Request, Counts, error) {
	r, err := scanRequest(s.db.QueryRow(ctx, `SELECT `+requestCols+` FROM deletion_requests WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, Counts{}, ErrNotFound
	}
	if err != nil {
		return Request{}, Counts{}, fmt.Errorf("storedanswers: read request: %w", err)
	}
	if r.Status == StatusDone {
		return r, Counts{}, ErrAlreadyDone
	}
	c, err := s.Delete(ctx, r.WorkspaceID, ScopeAll, by, id)
	if err != nil {
		return Request{}, Counts{}, err
	}
	r, err = scanRequest(s.db.QueryRow(ctx, `UPDATE deletion_requests
		SET status = 'done', completed_at = NOW(), completed_by = $2
		WHERE id = $1 AND status = 'requested' RETURNING `+requestCols, id, by))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, c, ErrAlreadyDone
	}
	if err != nil {
		return Request{}, c, fmt.Errorf("storedanswers: mark done: %w", err)
	}
	return r, c, nil
}
