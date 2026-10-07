package market

// B32.46 — a similarity check before publishing catches undeclared copies.
//
// Every version a publish adds is fingerprinted from its text — its listing's title and description and the string
// values of its artifact, in key order: an embedding (internal/embedder) and a MinHash of its five-word shingles, kept in
// market_listing_fingerprints (migration 0216). The version is compared with every fingerprinted version of the approved
// public listings and of the contributions of the rooms its publisher belongs to; its score against one is the larger of
// the embeddings' cosine and the share of shingles the MinHashes estimate the two have in common. The nearest listing at
// or above LENS_MARKET_SIMILARITY_HOLD that is neither a declared parent, an ancestor of one, the publisher's own nor a
// remix of the publisher's own HOLDS the version for review (Scan.Held), and the scan names it (Scan.Similar) with its
// score and whether it may be remixed: declaring it as a parent under a remix grant (B32.25) publishes the copy
// approved, and anything else waits for a person.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// DefaultSimilarityHold is the score at which a version is held as an undeclared copy (LENS_MARKET_SIMILARITY_HOLD):
// 0.92 — a proposal for Nicolai.
const DefaultSimilarityHold = 0.92

const (
	shingleWords  = 5      // the words in one MinHash shingle
	minhashSize   = 128    // the hashes in one MinHash
	maxEmbedBytes = 16_000 // the most of a version's text sent to the embedder, well inside its token limit
)

// Embedder turns a version's text into a vector (internal/embedder); Model names the space its vectors live in, so
// vectors of two models are never compared.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Model() string
}

// Similar is the listing a version is nearest to among those it may not copy without declaring it.
type Similar struct {
	ListingID string  `json:"listing_id"`
	Title     string  `json:"title"`
	Score     float64 `json:"score"`     // 0 to 1
	Remixable bool    `json:"remixable"` // it allows remixes: remix it and declare it as a parent to publish without a review
}

type similarityCheck struct {
	embedder Embedder
	hold     float64
}

// SetSimilarity turns the check on: e fingerprints each version and a score at or above hold holds it. Without it no
// version is fingerprinted or held as a copy.
func (s *Store) SetSimilarity(e Embedder, hold float64) {
	s.similarity = &similarityCheck{embedder: e, hold: hold}
}

// fingerprint is one version's: the embedding and its model (nil when the embedder failed or the text has no words) and
// the MinHash (nil when the text has no words).
type fingerprint struct {
	model     string
	embedding []float32
	minhash   []int64
}

// checkCopy fingerprints a version of a listing listingID ("" for a new one) published by publisher — into roomID when
// it is a room's contribution — declaring parents, and records on scan the nearest listing it copies without declaring
// it, holding the version when nothing else already holds it. It answers nil when the check is off.
func (s *Store) checkCopy(ctx context.Context, q rowQuerier, scan *Scan, artifact []byte, publisher, roomID string, parents []string,
	words ...string) (*fingerprint, error) {
	if s.similarity == nil {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(artifact, &obj); err != nil {
		return nil, fmt.Errorf("market: fingerprint: %w", err)
	}
	text := strings.Join(valuesIn(obj, words), "\n")
	fp := &fingerprint{minhash: minHash(text)}
	if v, err := s.similarity.embedder.Embed(ctx, truncated(text, maxEmbedBytes)); err != nil {
		slog.Warn("market: the similarity check compares wording only: the embedder failed", "error", err)
	} else if unitLength(v) {
		fp.model, fp.embedding = s.similarity.embedder.Model(), v
	}
	var embedding any
	if fp.embedding != nil {
		embedding = vectorText(fp.embedding)
	}
	var near Similar
	var policy string
	var fork bool
	err := q.QueryRow(ctx, `WITH RECURSIVE kin(id) AS (
				SELECT unnest($3::text[])
				UNION
				SELECT e.parent_listing_id FROM market_lineage e JOIN kin ON e.child_listing_id = kin.id),
			mine(id) AS (
				SELECT id FROM market_listings WHERE workspace_id = $1
				UNION
				SELECT e.child_listing_id FROM market_lineage e JOIN mine ON e.parent_listing_id = mine.id),
			scored AS (
				SELECT l.id, l.title, l.remix_policy, l.visibility = 'room' AND EXISTS (
						SELECT 1 FROM room_members m WHERE m.room_id = l.room_id AND m.workspace_id = $1 AND m.removed_at IS NULL) AS fork, GREATEST(
					CASE WHEN f.embedding_model = $4 AND f.embedding IS NOT NULL AND $5::vector IS NOT NULL THEN 1 - (f.embedding <=> $5::vector) END,
					(SELECT count(*) FROM unnest(f.minhash, $6::bigint[]) AS u(a, b) WHERE a = b)::float8 / $7) AS score
				FROM market_listing_fingerprints f JOIN market_listings l ON l.id = f.listing_id
				WHERE l.review_status = 'approved' AND l.workspace_id <> $1
				  AND (l.visibility = 'public' OR (l.visibility = 'room' AND (l.room_id = $2 OR EXISTS (
						SELECT 1 FROM room_members m WHERE m.room_id = l.room_id AND m.workspace_id = $1))))
				  AND NOT EXISTS (SELECT 1 FROM kin WHERE kin.id = l.id) AND NOT EXISTS (SELECT 1 FROM mine WHERE mine.id = l.id))
		SELECT id, title, remix_policy, fork, score FROM scored WHERE score >= $8 ORDER BY score DESC, id LIMIT 1`,
		publisher, roomID, parents, fp.model, embedding, fp.minhash, float64(minhashSize), s.similarity.hold).
		Scan(&near.ListingID, &near.Title, &policy, &fork, &near.Score)
	if errors.Is(err, pgx.ErrNoRows) {
		return fp, nil
	}
	if err != nil {
		return nil, fmt.Errorf("market: similarity: %w", err)
	}
	near.Score = math.Round(min(near.Score, 1)*10000) / 10000
	near.Remixable = fork || policy == RemixFree || policy == RemixRoyalty
	scan.Similar = &near
	if scan.Held == "" {
		scan.Held = heldAsCopy(near, fork)
	}
	return fp, nil
}

// parentIDs are the listings refs declares.
func parentIDs(refs []ParentRef) []string {
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ids = append(ids, r.ListingID)
	}
	return ids
}

// heldAsCopy is the reason a version near another seller's listing is held, and how to go on: a contribution to a room
// the publisher is a member of (fork) is declared as a parent without a remix grant (B32.31).
func heldAsCopy(near Similar, fork bool) string {
	held := fmt.Sprintf("it is %d%% similar to %q (%s), which it does not declare as a parent", int(math.Floor(near.Score*100)), near.Title, near.ListingID)
	switch {
	case fork:
		return held + ": that listing is a contribution to a room you are a member of — publish again declaring it as a parent, or wait for a review"
	case near.Remixable:
		return held + ": that listing allows remixes — remix it (POST /v1/workspaces/{ws}/marketplace/listings/" + near.ListingID +
			"/remix) and publish again declaring it as a parent, or wait for a review"
	}
	return held + " and which does not allow remixes: it waits for a review"
}

// writeFingerprint keeps a version's fingerprint for the publishes after it to be compared with.
func writeFingerprint(ctx context.Context, tx pgx.Tx, listingID string, version int, fp *fingerprint) error {
	if fp == nil {
		return nil
	}
	var embedding any
	if fp.embedding != nil {
		embedding = vectorText(fp.embedding)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO market_listing_fingerprints (listing_id, version, embedding_model, embedding, minhash)
		VALUES ($1, $2, $3, $4::vector, $5)`, listingID, version, fp.model, embedding, fp.minhash); err != nil {
		return fmt.Errorf("market: fingerprint: %w", err)
	}
	return nil
}

// valuesIn collects words, then every string value in a decoded JSON value, a map's in the order of its keys — its
// wording, not its layout, so the keys every listing of a kind shares are left out.
func valuesIn(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = valuesIn(e, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = valuesIn(x[k], out)
		}
	}
	return out
}

// minHash is the MinHash of text's five-word shingles (a text of fewer words is one shingle), nil for a text without
// words. Two MinHashes agree at about the share of shingles their texts have in common.
func minHash(text string) []int64 {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(words) == 0 {
		return nil
	}
	mins := make([]uint64, minhashSize)
	for i := range mins {
		mins[i] = math.MaxUint64
	}
	for i := 0; i+shingleWords <= max(len(words), shingleWords); i++ {
		h := fnv.New64a()
		_, _ = h.Write([]byte(strings.Join(words[i:min(i+shingleWords, len(words))], " ")))
		base := h.Sum64()
		for j := range mins {
			if v := mix64(base + uint64(j)*0x9e3779b97f4a7c15); v < mins[j] {
				mins[j] = v
			}
		}
	}
	out := make([]int64, minhashSize)
	for i, v := range mins {
		out[i] = int64(v)
	}
	return out
}

// mix64 is SplitMix64's finaliser: each of the MinHash's hashes is the shingle's hash, offset and mixed.
func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// unitLength says v is a vector that can be compared: not empty and not all zero.
func unitLength(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return true
		}
	}
	return false
}

// vectorText is v as pgvector's text form.
func vectorText(v []float32) string {
	b := make([]byte, 0, len(v)*10)
	b = append(b, '[')
	for i, x := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(x), 'g', -1, 32)
	}
	return string(append(b, ']'))
}

// truncated is s cut to at most n bytes, on a character's boundary.
func truncated(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
