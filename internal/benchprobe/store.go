package benchprobe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDuplicateItem is returned by ContributeItem when an item with the same content_hash already
// exists — the exact-dedup anti-farming reject at the contribution boundary.
var ErrDuplicateItem = errors.New("benchprobe: duplicate eval item (content_hash already present)")

// ErrUnknownItem is returned by AttestCorrectness when the target eval item does not exist.
var ErrUnknownItem = errors.New("benchprobe: eval item not found")

// ErrSelfAttestation is returned when a workspace attempts to attest the correctness of its OWN eval item
// — the literal-self reject at the boundary (the transitive same-operator exclusion is enforced deeper, in
// the mint's consensus gate, so sockpuppets are also caught; this is the cheap first line).
var ErrSelfAttestation = errors.New("benchprobe: a workspace cannot attest the correctness of its own eval item")

// ContentHash is the exact-dedup key over the item input — hex(sha256(input)), the same algorithm as
// distill.ContentHash (replicated locally so benchprobe stays dependency-free / mint-free).
func ContentHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// Store is the verifier-private pool + per-node score + never-reuse probe ledger over the 0068
// tables. It holds no ledger and reaches no mint path.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SeedItem inserts/updates one verifier-private pool item (the operator seed tool's only write).
func (s *Store) SeedItem(ctx context.Context, item EvalItem) error {
	method := item.EvalMethod
	if method == "" {
		method = "exact"
	}
	thr := item.PassThreshold
	if thr == 0 {
		thr = 1.0
	}
	// Operator-seeded items are immediately active and OWNERLESS (author NULL). content_hash is set so
	// operator seeds also dedup; status defaults 'active'. Cohort columns (PR-2) are stored as the caller
	// supplied them (the seed tool derives input_token_range/complexity_bucket via cohort.DeriveInputCohort
	// and declares feature_category); empty ⇒ NULL (untagged, not matchable).
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_eval_items (id, input, expected_output, eval_method, pass_threshold, active, content_hash, status, feature_category, input_token_range, complexity_bucket)
		 VALUES ($1,$2,$3,$4,$5,true,$6,'active',$7,$8,$9)
		 ON CONFLICT (id) DO UPDATE SET input=$2, expected_output=$3, eval_method=$4, pass_threshold=$5, content_hash=$6,
		   feature_category=$7, input_token_range=$8, complexity_bucket=$9`,
		item.ID, item.Input, item.ExpectedOutput, method, thr, ContentHash(item.Input),
		nullIfEmpty(item.FeatureCategory), nullIfEmpty(item.InputTokenRange), nullIfEmpty(item.ComplexityBucket))
	if err != nil {
		return fmt.Errorf("benchprobe: seed item: %w", err)
	}
	return nil
}

// nullIfEmpty maps an empty string to a SQL NULL so an untagged cohort dimension is NULL (not ”) —
// keeping "untagged ⇒ not matchable" exact (the cohort index is WHERE feature_category IS NOT NULL).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ContributeItem inserts a CONTRIBUTED eval item (proof-of-eval-contribution): authored, exact-deduped,
// and landing 'pending' (active=false) so it is NOT drawn or mint-eligible until an operator validates
// it to 'active'. A content_hash collision (a duplicate question) returns ErrDuplicateItem — the
// anti-farming reject at the contribution boundary, BEFORE the item can ever earn. AuthorWorkspaceID is
// required; it is verifier-private and never reaches a node (BuildProbeRequest reads only Input).
func (s *Store) ContributeItem(ctx context.Context, item EvalItem) error {
	if item.AuthorWorkspaceID == "" {
		return errors.New("benchprobe: contribute requires AuthorWorkspaceID")
	}
	method := item.EvalMethod
	if method == "" {
		method = "exact"
	}
	thr := item.PassThreshold
	if thr == 0 {
		thr = 1.0
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_eval_items (id, input, expected_output, eval_method, pass_threshold, active, content_hash, status, author_workspace_id, feature_category, input_token_range, complexity_bucket)
		 VALUES ($1,$2,$3,$4,$5,false,$6,'pending',$7,$8,$9,$10)`,
		item.ID, item.Input, item.ExpectedOutput, method, thr, ContentHash(item.Input), item.AuthorWorkspaceID,
		nullIfEmpty(item.FeatureCategory), nullIfEmpty(item.InputTokenRange), nullIfEmpty(item.ComplexityBucket))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation on idx_eval_items_content_hash
			return ErrDuplicateItem
		}
		return fmt.Errorf("benchprobe: contribute item: %w", err)
	}
	return nil
}

// AttestCorrectness records one INDEPENDENT workspace's judgment that an eval item's CLAIMED answer is
// correct (agrees=true) or disputed (agrees=false) — the correctness-consensus substrate the eval minter
// gates payment on. It is the second half of the live submission surface (ContributeItem submits the eval;
// other workspaces attest it). One vote per (item, attester): ON CONFLICT upserts, so re-attestation only
// updates the SAME operator's single vote and can never inflate the independent count.
//
// The literal author is rejected here (ErrSelfAttestation); the deeper transitive same-operator exclusion
// (an author's sockpuppets) is enforced by the mint-time consensus gate against the identity graph. Mint-
// free: this writes only eval_correctness_attestations (no ledger, no money).
func (s *Store) AttestCorrectness(ctx context.Context, itemID, attesterWorkspaceID string, agrees bool) error {
	if itemID == "" || attesterWorkspaceID == "" {
		return errors.New("benchprobe: attest requires item_id and attester_workspace_id")
	}
	var author *string // author_workspace_id is NULL for operator-seeded items
	err := s.pool.QueryRow(ctx, `SELECT author_workspace_id FROM benchmark_eval_items WHERE id=$1`, itemID).Scan(&author)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownItem
		}
		return fmt.Errorf("benchprobe: attest load item: %w", err)
	}
	if author != nil && *author == attesterWorkspaceID {
		return ErrSelfAttestation
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO eval_correctness_attestations (item_id, attester_workspace_id, agrees)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (item_id, attester_workspace_id) DO UPDATE SET agrees=$3, created_at=NOW()`,
		itemID, attesterWorkspaceID, agrees); err != nil {
		return fmt.Errorf("benchprobe: attest correctness: %w", err)
	}
	return nil
}

// NewItemID mints a fresh, unique eval-item id for the live submission surface (the endpoint generates one
// per submission). Prefixed for debuggability.
func NewItemID() string { return "eval_" + newID() }

// SubmitLiveEval is the LIVE submission surface's write (the endpoint's action, REPLACING the benchseed CLI).
// It lands the contributor's eval ACTIVE + author-stamped + content-deduped and returns the generated id.
// Symmetric with the routing EmitLivePrediction: a live contribution is drawable immediately; EARNING is
// gated downstream by correctness consensus + the usage warmup + the U6 floor (an active-but-unconsensed eval
// mints nothing). A content_hash collision returns ErrDuplicateItem (the exact-dedup anti-farm reject).
func (s *Store) SubmitLiveEval(ctx context.Context, item EvalItem) (string, error) {
	if item.AuthorWorkspaceID == "" {
		return "", errors.New("benchprobe: submit requires AuthorWorkspaceID")
	}
	if item.Input == "" || item.ExpectedOutput == "" {
		return "", errors.New("benchprobe: submit requires input and expected_output")
	}
	if item.ID == "" {
		item.ID = NewItemID()
	}
	method := item.EvalMethod
	if method == "" {
		method = "exact"
	}
	thr := item.PassThreshold
	if thr == 0 {
		thr = 1.0
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_eval_items (id, input, expected_output, eval_method, pass_threshold, active, content_hash, status, author_workspace_id, feature_category, input_token_range, complexity_bucket)
		 VALUES ($1,$2,$3,$4,$5,true,$6,'active',$7,$8,$9,$10)`,
		item.ID, item.Input, item.ExpectedOutput, method, thr, ContentHash(item.Input), item.AuthorWorkspaceID,
		nullIfEmpty(item.FeatureCategory), nullIfEmpty(item.InputTokenRange), nullIfEmpty(item.ComplexityBucket))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation on idx_eval_items_content_hash
			return "", ErrDuplicateItem
		}
		return "", fmt.Errorf("benchprobe: submit live eval: %w", err)
	}
	return item.ID, nil
}

// BackfillCohort sets the two DERIVED cohort dimensions (input_token_range, complexity_bucket) on an
// existing item — used by the seed tool's --backfill pass to tag legacy rows from their stored input.
// feature_category is NOT touched (it is declared, never derived; it stays NULL until re-seeded).
func (s *Store) BackfillCohort(ctx context.Context, id, inputTokenRange, complexityBucket string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE benchmark_eval_items SET input_token_range=$2, complexity_bucket=$3 WHERE id=$1`,
		id, nullIfEmpty(inputTokenRange), nullIfEmpty(complexityBucket))
	if err != nil {
		return fmt.Errorf("benchprobe: backfill cohort: %w", err)
	}
	return nil
}

// ItemsMissingCohort returns (id, input) for items whose derived cohort is not yet set — the --backfill
// work-list. Read-only; the tool re-derives and calls BackfillCohort per row.
func (s *Store) ItemsMissingCohort(ctx context.Context, limit int) ([]struct{ ID, Input string }, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, input FROM benchmark_eval_items WHERE input_token_range IS NULL OR complexity_bucket IS NULL LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("benchprobe: items missing cohort: %w", err)
	}
	defer rows.Close()
	var out []struct{ ID, Input string }
	for rows.Next() {
		var r struct{ ID, Input string }
		if err := rows.Scan(&r.ID, &r.Input); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ValidateItem flips a contributed item from 'pending' to 'active' (operator-mediated validation), so
// it becomes drawable and — once it accumulates ≥ MinUnlinkedGraders distinct unlinked graders —
// mint-eligible. Quarantine uses status='quarantined' (never drawn, never paid).
func (s *Store) ValidateItem(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE benchmark_eval_items SET active=true, status='active' WHERE id=$1 AND status='pending'`, id)
	if err != nil {
		return fmt.Errorf("benchprobe: validate item: %w", err)
	}
	return nil
}

// DrawItem picks an UNPREDICTABLE active item NOT yet probed to nodeID. It loads the candidate id set
// (active, never-probed-to-this-node) and selects one via crypto/rand (the unpredictability source,
// mirroring the PoVI challenger) so the node cannot anticipate which item it will be asked. Returns
// nil when the pool is exhausted for this node (no item left to draw) — the caller treats nil as a
// no-op, never an error.
func (s *Store) DrawItem(ctx context.Context, nodeID string) (*EvalItem, error) {
	// Author-exclusion (proof-of-eval-contribution sockpuppet defense): an item is NEVER drawn for a
	// node whose owning workspace is the item's author OR in the author's owner-linkage fingerprint-
	// linked set (the SAME workspace_card_fingerprints self-deal join the royalty minter uses,
	// minter.go:120). So an author can neither grade their own item nor grade it via a same-card sister
	// workspace's node. RESIDUAL (blessed bound, not eliminated): a different-card/no-card sock evades
	// the fingerprint link (default-allow on missing) and CAN grade — bounded downstream by the
	// MinUnlinkedGraders warmup + the U6 24h author cap (a logged pre-public-mint gate). status='active'
	// also drops 'pending'/'quarantined' contributed items. Operator-seeded items (author NULL) are
	// never excluded. The probe path is the ONLY way an item reaches a node.
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM benchmark_eval_items e
		 WHERE e.active AND e.status = 'active'
		   AND e.id NOT IN (SELECT item_id FROM benchmark_probes WHERE node_id = $1)
		   AND (e.author_workspace_id IS NULL OR (
		         e.author_workspace_id <> (SELECT workspace_id FROM inference_nodes WHERE id = $1)
		         AND e.author_workspace_id NOT IN (
		             SELECT b.workspace_id FROM workspace_card_fingerprints a
		             JOIN workspace_card_fingerprints b ON a.fingerprint_hash = b.fingerprint_hash
		             WHERE a.workspace_id = (SELECT workspace_id FROM inference_nodes WHERE id = $1))))`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("benchprobe: draw candidates: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil // pool exhausted for this node
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(ids))))
	if err != nil {
		return nil, fmt.Errorf("benchprobe: crypto/rand draw: %w", err)
	}
	pick := ids[n.Int64()]

	var it EvalItem
	if err := s.pool.QueryRow(ctx,
		`SELECT id, input, expected_output, eval_method, pass_threshold FROM benchmark_eval_items WHERE id=$1`, pick).
		Scan(&it.ID, &it.Input, &it.ExpectedOutput, &it.EvalMethod, &it.PassThreshold); err != nil {
		return nil, fmt.Errorf("benchprobe: load item: %w", err)
	}
	return &it, nil
}

// RecordProbe inserts the never-reuse ledger row. ON CONFLICT (node_id, item_id) DO NOTHING makes a
// double-draw race idempotent (the UNIQUE constraint is the real never-reuse guarantee).
func (s *Store) RecordProbe(ctx context.Context, p Probe) error {
	id := p.ID
	if id == "" {
		id = newID()
	}
	var reqID any
	if p.RequestID != "" {
		reqID = p.RequestID
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_probes (id, node_id, item_id, request_id, score)
		 VALUES ($1,$2,$3,$4,$5) ON CONFLICT (node_id, item_id) DO NOTHING`,
		id, p.NodeID, p.ItemID, reqID, p.Score)
	if err != nil {
		return fmt.Errorf("benchprobe: record probe: %w", err)
	}
	return nil
}

// SetProbeScore updates the score on an already-committed probe row (after the response). The row is
// inserted BEFORE delivery (happens-before), so this only fills in the score.
func (s *Store) SetProbeScore(ctx context.Context, requestID string, score float64) error {
	_, err := s.pool.Exec(ctx, `UPDATE benchmark_probes SET score=$2 WHERE request_id=$1`, requestID, score)
	if err != nil {
		return fmt.Errorf("benchprobe: set probe score: %w", err)
	}
	return nil
}

// IsProbe is the gateway-side mint-suppression lookup: does this receipt request_id belong to a
// verifier-induced probe? A POINT existence check on the idx_benchmark_probes_request index (not a
// scan). Wired into poviProcessor as the probe-mint suppression (record-but-skip-mint).
func (s *Store) IsProbe(ctx context.Context, requestID string) (bool, error) {
	if requestID == "" {
		return false, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM benchmark_probes WHERE request_id=$1)`, requestID).Scan(&exists); err != nil {
		return false, fmt.Errorf("benchprobe: probe lookup: %w", err)
	}
	return exists, nil
}

// NewProbeRequestID mints a fresh, unique probe request id (the X-Request-ID an honest node echoes
// into its receipt; the suppression key). Prefixed for debuggability.
func NewProbeRequestID() string { return "bench_" + newID() }

// UpsertNodeScore folds one probe score into the node's running average for that model.
func (s *Store) UpsertNodeScore(ctx context.Context, nodeID, model string, score float64) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO benchmark_node_scores (node_id, model, score, sample_count, updated_at)
		 VALUES ($1,$2,$3,1,NOW())
		 ON CONFLICT (node_id, model) DO UPDATE SET
		   score = (benchmark_node_scores.score * benchmark_node_scores.sample_count + $3)
		           / (benchmark_node_scores.sample_count + 1),
		   sample_count = benchmark_node_scores.sample_count + 1,
		   updated_at = NOW()`,
		nodeID, model, score)
	if err != nil {
		return fmt.Errorf("benchprobe: upsert node score: %w", err)
	}
	return nil
}

// NodeScore reads the current per-node score + sample count (for routing in PR-B + tests). 0,0 when absent.
func (s *Store) NodeScore(ctx context.Context, nodeID, model string) (score float64, sampleCount int, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT score, sample_count FROM benchmark_node_scores WHERE node_id=$1 AND model=$2`, nodeID, model).
		Scan(&score, &sampleCount)
	if err != nil {
		return 0, 0, nil // absent ⇒ unscored
	}
	return score, sampleCount, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
