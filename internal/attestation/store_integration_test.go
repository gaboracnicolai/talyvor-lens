package attestation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG attestation store test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS node_attestations (
		nonce BIGINT PRIMARY KEY, node_id TEXT NOT NULL, attestation_status TEXT NOT NULL DEFAULT 'pending',
		attested_gpu_class TEXT, cc_mode BOOLEAN, eat_digest TEXT, key_bound BOOLEAN NOT NULL DEFAULT false,
		attested_at TIMESTAMPTZ NOT NULL DEFAULT now(), expires_at TIMESTAMPTZ)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE node_attestations`); err != nil {
		t.Fatal(err)
	}
	return &Store{db: pool}, pool
}

var verified = ConsumeResult{Status: "verified", GPUClass: "H100", CCMode: true, EATDigest: "d", KeyBound: true, ValidFor: 24 * time.Hour}

// (proof 2a) REPLAY: consuming the same issued nonce twice ⇒ 2nd REJECTS (single-use CAS).
func TestStore_Replay_SingleUse(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	if err := s.IssueNonce(ctx, 111, "nodeA"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Consume(ctx, 111, "nodeA", verified, time.Hour)
	if err != nil || !ok {
		t.Fatalf("first consume must win: ok=%v err=%v", ok, err)
	}
	ok2, _ := s.Consume(ctx, 111, "nodeA", verified, time.Hour)
	if ok2 {
		t.Fatal("REPLAY: second consume of the same nonce must REJECT (single-use)")
	}
	var status string
	var kb bool
	_ = pool.QueryRow(ctx, `SELECT attestation_status, key_bound FROM node_attestations WHERE nonce=111`).Scan(&status, &kb)
	if status != "verified" || !kb {
		t.Fatalf("row must be verified+key_bound: status=%s kb=%v", status, kb)
	}
}

// (proof 2b) CROSS-NODE / RELAY-PRESENTATION: a nonce issued to node A, consumed FOR node B ⇒ REJECT (the
// CAS node_id binding). A's row stays pending.
func TestStore_CrossNode_Binding(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	if err := s.IssueNonce(ctx, 222, "nodeA"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Consume(ctx, 222, "nodeB", verified, time.Hour) // wrong node
	if err != nil || ok {
		t.Fatalf("nonce issued to A must NOT consume for B: ok=%v err=%v", ok, err)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT attestation_status FROM node_attestations WHERE nonce=222`).Scan(&status)
	if status != "pending" {
		t.Fatalf("A's nonce must remain pending after a wrong-node consume, got %s", status)
	}
	// A can still consume its own nonce.
	if ok, _ := s.Consume(ctx, 222, "nodeA", verified, time.Hour); !ok {
		t.Fatal("node A must still consume its own nonce")
	}
}

// (proof 2b') WINDOW: a pending nonce past the challenge window ⇒ REJECT.
func TestStore_ChallengeWindow_Expiry(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	if err := s.IssueNonce(ctx, 333, "nodeA"); err != nil {
		t.Fatal(err)
	}
	// backdate the issue so it's outside a 1s window.
	if _, err := pool.Exec(ctx, `UPDATE node_attestations SET attested_at = now() - interval '10 seconds' WHERE nonce=333`); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Consume(ctx, 333, "nodeA", verified, time.Second); ok {
		t.Fatal("a nonce past the challenge window must REJECT")
	}
}

// (proof 3-store) a FAILED verify records status=failed and does NOT set a validity/class (nothing for the
// mint to read).
func TestStore_FailedVerify_RecordsFailed(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	_ = s.IssueNonce(ctx, 444, "nodeA")
	failed := ConsumeResult{Status: "failed"}
	if ok, _ := s.Consume(ctx, 444, "nodeA", failed, time.Hour); !ok {
		t.Fatal("failed verify still consumes the nonce (single-use), got not-consumed")
	}
	var status string
	var class *string
	_ = pool.QueryRow(ctx, `SELECT attestation_status, attested_gpu_class FROM node_attestations WHERE nonce=444`).Scan(&status, &class)
	if status != "failed" || class != nil {
		t.Fatalf("failed row must have no gpu_class: status=%s class=%v", status, class)
	}
}
