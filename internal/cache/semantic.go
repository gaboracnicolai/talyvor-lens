package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/discriminator"
	"github.com/talyvor/lens/internal/doc2query"
	"github.com/talyvor/lens/internal/metrics"
)

// Embedder turns a text prompt into an embedding vector.
// Implemented in other packages (e.g. an OpenAI-backed embedder).
//
// Model reports which embedding model produced those vectors, and it is part of this
// interface DELIBERATELY. Provenance has to come from the thing that made the vector: a
// model name passed alongside the embedder is a second source of truth that can silently
// disagree with it, and a disagreement here is unobservable — see migrations/0110. Every
// implementation is therefore forced by the compiler to state its identity, and no caller
// can construct a cache that writes vectors of unknown origin.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Model() string
}

// SemanticDB is the subset of *pgxpool.Pool that SemanticCache needs. Exported
// so tests (including in other packages, e.g. the proxy) can substitute a
// pgxmock pool without a real database.
type SemanticDB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type SemanticCache struct {
	pool      SemanticDB
	embedder  Embedder
	threshold float64
	// embeddingModel identifies the embedder that produces this cache's vectors. It is
	// stamped on every row written and required to match on every row read. Empty means
	// "unknown", which is treated as not-comparable rather than as a wildcard — see
	// migrations/0110.
	embeddingModel string
	// retention is the single sliding window for a semantic-cache row. It is
	// BOTH the serve window (freshnessCutoff gates semanticSelectSQL on
	// updated_at > NOW() − retention) AND the deletion window (DeleteStale
	// removes rows past it). A row's updated_at is bumped to NOW() on every
	// served hit (semanticTouchSQL), so the window restarts on each use — an
	// entry stays alive AND servable as long as it is used at least once per
	// window. retention <= 0 disables both halves: rows are served regardless of
	// age and never swept (kept indefinitely).
	retention time.Duration
}

func NewSemanticCache(pool *pgxpool.Pool, embedder Embedder, threshold float64, retention time.Duration) *SemanticCache {
	return newSemanticCache(pool, embedder, threshold, retention)
}

// NewSemanticCacheWithDB builds a SemanticCache over any SemanticDB (e.g. a
// pgxmock pool in tests).
func NewSemanticCacheWithDB(pool SemanticDB, embedder Embedder, threshold float64, retention time.Duration) *SemanticCache {
	return newSemanticCache(pool, embedder, threshold, retention)
}

func newSemanticCache(pool SemanticDB, embedder Embedder, threshold float64, retention time.Duration) *SemanticCache {
	// Provenance is READ OFF THE EMBEDDER, not accepted as a parameter. There is no call
	// site that can supply a name disagreeing with the model that actually produced the
	// vectors, because there is no call site that supplies one at all.
	m := embedder.Model()
	if m == "" {
		// An implementation that will not name itself cannot have its vectors compared
		// safely. Write NULL rather than a fabricated name, which makes the rows
		// unservable (see semanticSelectSQL) — the cache degrades to always-miss instead
		// of silently mixing spaces. Loud, because this is a programming error in an
		// Embedder implementation, not an operator setting.
		slog.Error("semantic cache: embedder reports an empty Model(); vectors will be written "+
			"without provenance and can never be served",
			"embedder", reflect.TypeOf(embedder).String())
	}
	return &SemanticCache{pool: pool, embedder: embedder, threshold: threshold, retention: retention, embeddingModel: m}
}

// semanticSelectSQL is the PRIVATE (workspace-scoped) lookup. The
// `updated_at > $4` clause is the SLIDING serve window: $4 is the freshness
// cutoff (NOW() − retention, computed in Go by freshnessCutoff). A row therefore
// stays servable for the full retention window, and every served hit bumps
// updated_at (semanticTouchSQL), which resets that window — so "servable" and
// "retained" are the SAME boundary the sweeper (DeleteStale) deletes at. When
// retention is disabled the cutoff is the zero time, so the filter is a no-op
// and every row is servable regardless of age. The `is_poolable = false` filter
// excludes shared-pool rows so a private lookup can never serve a pooled entry
// (which would bypass the cross-tenant consent check); it is a no-op when
// pooling is off (every row is is_poolable=false by default). The
// `workspace_id = $5` filter (#142) is the HARD tenant boundary: the embedding
// is only the similarity RANKER, so without this clause isolation rested purely
// on the wsID: prefix shifting the embedding past threshold (soft for long
// prompts). A NULL-workspace row (pre-#142, never re-stamped) matches no caller
// and is correctly excluded — cold, self-healing.
// The embedding_model equality is the corruption guard (migrations/0110). It IGNORES rows
// from a different embedder rather than erroring, because a vector from another model is
// genuinely NOT A MATCH — filtering it is the correct semantics, not a degradation. The
// failure mode is a temporary miss that goes upstream, returns the right answer, and
// re-caches under the current embedder: self-healing, and never a wrong answer. Erroring
// would take caching out entirely on an ordinary config change and would be treating a
// legitimate non-match as a fault.
//
// `= $N` also excludes legacy NULL rows by construction (NULL = x is never true), which is
// the intended treatment of unknown provenance.
const semanticSelectSQL = `SELECT id, response, 1 - (embedding <=> $1) AS similarity
FROM prompt_embeddings
WHERE provider = $2 AND model = $3
  AND updated_at > $4
  AND is_poolable = false
  AND workspace_id = $5
  AND embedding_model = $6
ORDER BY embedding <=> $1
LIMIT 1`

// semanticSelectPooledSQL is the SHARED-POOL lookup: it ranges ONLY over
// is_poolable=true rows and returns the contributing workspace so the caller can
// verify the contributor's live opt-in. The `updated_at > $4` serve window and
// its cutoff are identical to the private path (see semanticSelectSQL). COALESCE
// makes a missing contributor an empty string (→ the gate blocks it).
const semanticSelectPooledSQL = `SELECT COALESCE(variant_of, id) AS entry_id, response, COALESCE(contributor_workspace_id, '') AS contributor, 1 - (embedding <=> $1) AS similarity
FROM prompt_embeddings
WHERE provider = $2 AND model = $3
  AND embedding_model = $5
  AND updated_at > $4
  AND is_poolable = true
  -- ⚠ THE ENTITY GATE. Similarity judges topic; this judges identity. Pydantic v1 and v2 score
  -- 0.9579 — the vector distance cannot tell them apart and no threshold can. Equality here is
  -- also what fails legacy rows closed: their discriminators are NULL, and NULL = $6 is NULL,
  -- never TRUE, so a row whose prompt text no longer exists is never served.
  AND discriminators = $6
ORDER BY embedding <=> $1
LIMIT 1`

const semanticTouchSQL = `UPDATE prompt_embeddings
SET hit_count = hit_count + 1, updated_at = NOW()
WHERE id = $1`

// semanticDeleteStaleSQL removes every row whose last-use timestamp
// (updated_at, bumped on each hit by semanticTouchSQL) is older than the
// caller-supplied cutoff. The cutoff is computed in Go (NOW()-retention) rather
// than via a SQL INTERVAL so the retention window is a single parameterized
// timestamp — clean under PgBouncer transaction pooling / simple protocol. The
// filter is on updated_at alone, so it applies uniformly to private AND pooled
// (is_poolable) rows.
const semanticDeleteStaleSQL = `DELETE FROM prompt_embeddings WHERE updated_at < $1`

const semanticUpsertSQL = `INSERT INTO prompt_embeddings
  (provider, model, prompt_hash, embedding, response, workspace_id, embedding_model)
VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''))
ON CONFLICT (prompt_hash) DO UPDATE SET
  response = EXCLUDED.response,
  embedding = EXCLUDED.embedding,
  workspace_id = EXCLUDED.workspace_id,
  embedding_model = EXCLUDED.embedding_model,
  updated_at = NOW()`

// semanticUpsertPooledSQL writes a shared-pool row: contributor stamped,
// is_poolable=true (a literal). Its prompt_hash is keyed on a NUL-sentinel-
// prefixed prompt (the caller's job), provably disjoint from any private hash.
const semanticUpsertPooledSQL = `INSERT INTO prompt_embeddings
  (provider, model, prompt_hash, embedding, response, contributor_workspace_id, is_poolable, embedding_model, discriminators)
VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), $8)
ON CONFLICT (prompt_hash) DO UPDATE SET
  response = EXCLUDED.response,
  embedding = EXCLUDED.embedding,
  contributor_workspace_id = EXCLUDED.contributor_workspace_id,
  is_poolable = true,
  -- The overwriting vector comes from THIS process's embedder, so the provenance must
  -- follow it. Leaving the old value would label a new-model vector with the old model.
  embedding_model = EXCLUDED.embedding_model,
  -- Same reasoning for the entities: the row now answers the NEW prompt, so it must be
  -- findable by that prompt's entities and not the previous one's.
  discriminators = EXCLUDED.discriminators,
  updated_at = NOW()`

// freshnessCutoff is the lower bound a row's updated_at must exceed to remain
// servable: NOW() − retention. Because a served hit bumps updated_at, the window
// slides forward on every use, so an entry stays servable exactly as long as it
// is used at least once per retention window — the same boundary DeleteStale
// deletes at, keeping the serve and storage windows identical. When retention is
// disabled (<= 0) it returns the zero time, so `updated_at > cutoff` is always
// true and every row is servable (mirroring DeleteStale's keep-forever no-op).
func (c *SemanticCache) freshnessCutoff() time.Time {
	if c.retention <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(-c.retention)
}

func (c *SemanticCache) Get(ctx context.Context, provider, model, prompt, workspaceID string) ([]byte, error) {
	vec, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, err
	}

	var (
		id         string
		response   string
		similarity float64
	)
	// workspace_id is the HARD tenant filter (#142): a private lookup can only
	// match the caller's own rows; the embedding ranks within that boundary.
	err = c.pool.QueryRow(ctx, semanticSelectSQL, vectorLiteral(vec), provider, model, c.freshnessCutoff(), workspaceID, c.embeddingModel).
		Scan(&id, &response, &similarity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if similarity < c.threshold {
		return nil, nil
	}

	if _, err := c.pool.Exec(ctx, semanticTouchSQL, id); err != nil {
		return nil, err
	}

	metrics.CacheHitsTotal.WithLabelValues("semantic").Inc()
	return []byte(response), nil
}

func (c *SemanticCache) Set(ctx context.Context, provider, model, prompt string, response []byte, embedding []float32, workspaceID string) error {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	hash := hex.EncodeToString(sum[:])

	// prompt_hash is unchanged (still sha256 of the wsID-prefixed prompt — the
	// ON CONFLICT idempotency key); workspace_id is the ADDITIONAL hard-filter
	// column the private read scopes on (#142).
	_, err := c.pool.Exec(
		ctx,
		semanticUpsertSQL,
		provider, model, hash, vectorLiteral(embedding), string(response), workspaceID, c.embeddingModel,
	)
	return err
}

// SetPooled writes a SHARED-POOL row (is_poolable=true) tagged with the
// contributing workspace. The caller supplies a `prompt` already prefixed with
// the NUL-sentinel pooled marker, so the row's prompt_hash is provably disjoint
// from any workspace-private hash (which carries a "wsID:" prefix). Used only on
// the opt-in path — Stage 2.0b's cross-tenant write surface.
func (c *SemanticCache) SetPooled(ctx context.Context, provider, model, prompt, contributorWsID string, response []byte, embedding []float32) error {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	hash := hex.EncodeToString(sum[:])

	_, err := c.pool.Exec(
		ctx,
		semanticUpsertPooledSQL,
		provider, model, hash, vectorLiteral(embedding), string(response), contributorWsID, c.embeddingModel,
		string(discriminator.Canon(prompt)),
	)
	return err
}

// SetPooledWithVariants writes a pooled answer plus doc2query match targets for it.
//
// ⚠ EVERY VARIANT ROW COPIES THE ORIGINAL PROMPT'S DISCRIMINATORS. It never canonicalises its own
// text, and the one-line difference is the entire safety argument for this feature.
//
// A model asked to derive questions from a Pydantic v2 answer reliably produces version-less
// phrasings — "how do I validate a field?" — because the version is context it already has. If
// such a variant carried the entities of its OWN text it would be a match target with no version
// constraint pointing at version-specific prose, and the first person to ask the unversioned
// question would be served v2 content with a royalty paid on it. That is the hole migration 0112
// closed, re-opened by the safest-looking line of code in the package.
//
// Inheriting confines doc2query to widening recall INSIDE an entity class, which is the only place
// it is safe: it can find you a different phrasing of a Pydantic v2 question, and can never find
// you a Pydantic v1 one.
func (c *SemanticCache) SetPooledWithVariants(ctx context.Context, provider, model, prompt, contributorWsID string, response []byte, embedding []float32, variants []doc2query.Variant) error {
	sum := sha256.Sum256([]byte(provider + ":" + model + ":" + prompt))
	hash := hex.EncodeToString(sum[:])

	var originalID string
	if err := c.pool.QueryRow(ctx, semanticUpsertPooledReturningSQL,
		provider, model, hash, vectorLiteral(embedding), string(response), contributorWsID, c.embeddingModel,
		string(discriminator.Canon(prompt)),
	).Scan(&originalID); err != nil {
		return err
	}

	// Computed ONCE, from the original, and reused for every variant — so there is no code path
	// on which a variant's own text can reach the discriminators column.
	inherited := string(discriminator.Canon(prompt))
	for _, v := range variants {
		vsum := sha256.Sum256([]byte(provider + ":" + model + ":variant:" + v.Question))
		if _, err := c.pool.Exec(ctx, semanticUpsertVariantSQL,
			provider, model, hex.EncodeToString(vsum[:]), vectorLiteral(v.Embedding), string(response),
			contributorWsID, c.embeddingModel, inherited, originalID,
		); err != nil {
			return err
		}
	}
	return nil
}

const semanticUpsertPooledReturningSQL = semanticUpsertPooledSQL + `
RETURNING id`

const semanticUpsertVariantSQL = `INSERT INTO prompt_embeddings
  (provider, model, prompt_hash, embedding, response, contributor_workspace_id, is_poolable, embedding_model, discriminators, variant_of)
VALUES ($1, $2, $3, $4, $5, $6, true, NULLIF($7, ''), $8, $9)
ON CONFLICT (prompt_hash) DO UPDATE SET
  response = EXCLUDED.response,
  embedding = EXCLUDED.embedding,
  contributor_workspace_id = EXCLUDED.contributor_workspace_id,
  embedding_model = EXCLUDED.embedding_model,
  discriminators = EXCLUDED.discriminators,
  variant_of = EXCLUDED.variant_of,
  updated_at = NOW()`

// GetPooled is the cross-tenant similarity lookup: it searches ONLY is_poolable
// rows and returns the cached response, the contributing workspace, the matched
// row's prompt_embeddings.id, and the similarity score. A miss (no row, or
// below threshold) is (nil, "", "", 0, nil). The contributor lets the caller
// verify the contributor's live opt-in before serving; an empty contributor
// (defensive — should not occur for a poolable row) surfaces as "" so the gate
// blocks it. The entry id + similarity are Stage-2.1 attribution data for the
// royalty claim row — NOT an idempotency key (a retried request can re-match a
// different row: ORDER BY similarity LIMIT 1 over a moving 24h window).
func (c *SemanticCache) GetPooled(ctx context.Context, provider, model, prompt string) ([]byte, string, string, float64, error) {
	vec, err := c.embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, "", "", 0, err
	}

	var (
		id          string
		response    string
		contributor string
		similarity  float64
	)
	err = c.pool.QueryRow(ctx, semanticSelectPooledSQL, vectorLiteral(vec), provider, model, c.freshnessCutoff(), c.embeddingModel,
		string(discriminator.Canon(prompt))).
		Scan(&id, &response, &contributor, &similarity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", "", 0, nil
	}
	if err != nil {
		return nil, "", "", 0, err
	}
	if similarity < c.threshold {
		return nil, "", "", 0, nil
	}
	if _, err := c.pool.Exec(ctx, semanticTouchSQL, id); err != nil {
		return nil, "", "", 0, err
	}

	metrics.CacheHitsTotal.WithLabelValues("semantic_pooled").Inc()
	return []byte(response), contributor, id, similarity, nil
}

// DeleteStale removes semantic-cache rows that haven't been used within the
// retention window — the "delete if unused for the whole window" half of the
// sliding-timer retention (the reset-on-use half already happens via
// semanticTouchSQL bumping updated_at on every hit). It returns the number of
// rows deleted. A non-positive retention disables sweeping: it is a no-op that
// touches the database not at all and returns (0, nil). The cutoff is computed
// in Go (a single timestamp parameter) so it stays simple-protocol-safe under
// PgBouncer transaction pooling.
func (c *SemanticCache) DeleteStale(ctx context.Context) (int64, error) {
	if c.retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-c.retention)
	tag, err := c.pool.Exec(ctx, semanticDeleteStaleSQL, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// StartSweeper runs DeleteStale on a fixed interval until ctx is cancelled,
// mirroring the warmer's background-loop convention. The first sweep fires
// after one interval (not immediately) so process startup stays light. When
// retention is disabled (<= 0) the loop never starts — it logs once and
// returns, so the caller can launch it unconditionally as a goroutine. Sweep
// errors are logged and swallowed so one failed sweep can't kill the loop.
func (c *SemanticCache) StartSweeper(ctx context.Context, interval time.Duration) {
	if c.retention <= 0 {
		slog.Info("semantic cache retention sweeper disabled",
			slog.String("source", "semantic_cache_sweeper"),
			slog.String("reason", "retention <= 0"),
		)
		return
	}
	slog.Info("semantic cache retention sweeper started",
		slog.String("source", "semantic_cache_sweeper"),
		slog.Duration("retention", c.retention),
		slog.Duration("interval", interval),
	)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := c.DeleteStale(ctx)
			if err != nil {
				slog.Warn("semantic cache sweep failed",
					slog.String("source", "semantic_cache_sweeper"),
					slog.String("err", err.Error()),
				)
				continue
			}
			if deleted > 0 {
				slog.Info("semantic cache sweep complete",
					slog.String("source", "semantic_cache_sweeper"),
					slog.Int64("deleted", deleted),
				)
			}
		}
	}
}

// vectorLiteral encodes a vector in pgvector's text format: "[v1,v2,...]".
func vectorLiteral(v []float32) string {
	var sb strings.Builder
	sb.Grow(len(v)*8 + 2)
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
