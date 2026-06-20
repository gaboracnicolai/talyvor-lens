package povi_test

import (
	"context"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/povi"
	"github.com/talyvor/lens/migrations"
)

const leafKindSchema = "lens_l1_leafkind"

// preLeafKindReceipts is the povi_receipts shape BEFORE migration 0060 — so the test
// proves 0060 is an additive column add onto an existing, populated rune-rooted table.
const preLeafKindReceipts = `CREATE TABLE povi_receipts (
    request_id    TEXT PRIMARY KEY,
    node_id       TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    model         TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    merkle_root   TEXT NOT NULL,
    verified      BOOLEAN NOT NULL,
    timestamp     BIGINT NOT NULL,
    leaf_count    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// leafKindEnv sets up a fresh PRIVATE SCHEMA (the Lens gated-test convention — no
// server-level CREATE DATABASE that would disrupt the parallel shared-DB suite),
// inline-creates the pre-0060 povi_receipts, then applies the REAL migration 0060
// through the dbmigrate runner. Asserts 0060 was applied by the runner, and returns a
// Store pinned to the schema. Skips without LENS_TEST_DATABASE_URL.
func leafKindEnv(t *testing.T) (*povi.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG leaf_kind test")
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = leafKindSchema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, ddl := range []string{
		`DROP SCHEMA IF EXISTS ` + leafKindSchema + ` CASCADE`,
		`CREATE SCHEMA ` + leafKindSchema,
		preLeafKindReceipts,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("schema setup: %v", err)
		}
	}

	// Apply the REAL 0060 through the runner, into this schema (via a pool conn whose
	// search_path is already pinned to the schema).
	data, err := migrations.FS.ReadFile("0060_povi_leaf_kind.sql")
	if err != nil {
		t.Fatalf("read 0060: %v", err)
	}
	ac, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	applied, err := dbmigrate.Run(ctx, ac.Conn(), fstest.MapFS{"0060_povi_leaf_kind.sql": {Data: data}})
	ac.Release()
	if err != nil {
		t.Fatalf("dbmigrate.Run(0060): %v", err)
	}
	if len(applied) != 1 || !strings.HasPrefix(applied[0], "0060") {
		t.Fatalf("expected 0060 applied via runner, got %v", applied)
	}

	return povi.NewStore(pool), pool
}

// TestMigration0060_AppliesAndOldRowStillValidates — 0060 is applied by the runner; the
// leaf_kind column exists; an OLD-style row inserted without leaf_kind defaults to
// 'rune'; and its rune-rooted MerkleRoot is untouched — a proof over the original
// leaves still verifies (the tag distinguishes, it does not invalidate).
func TestMigration0060_AppliesAndOldRowStillValidates(t *testing.T) {
	store, pool := leafKindEnv(t)
	ctx := context.Background()

	var col string
	if err := pool.QueryRow(ctx,
		`SELECT column_name FROM information_schema.columns
         WHERE table_schema=$1 AND table_name='povi_receipts' AND column_name='leaf_kind'`,
		leafKindSchema).Scan(&col); err != nil {
		t.Fatalf("leaf_kind column missing after 0060: %v", err)
	}

	// An OLD rune-rooted receipt: a real Merkle root over rune leaves, inserted the
	// pre-0060 way (no leaf_kind in the INSERT).
	runeSteps := povi.StepsFromRunes("héllo wörld")
	root := povi.MerkleRoot(runeSteps)
	rootHex := hex.EncodeToString(root[:])
	if _, err := pool.Exec(ctx,
		`INSERT INTO povi_receipts
            (request_id, node_id, workspace_id, model, input_tokens, output_tokens, merkle_root, verified, timestamp, leaf_count)
         VALUES ('old-req','n','ws','m',1,2,$1,true,100,$2)`,
		rootHex, len(runeSteps)); err != nil {
		t.Fatalf("old-style insert: %v", err)
	}

	got, err := store.GetReceipt(ctx, "old-req")
	if err != nil {
		t.Fatalf("GetReceipt: %v", err)
	}
	if got.LeafKind != "rune" {
		t.Errorf("old row leaf_kind = %q, want 'rune' (the default backfill)", got.LeafKind)
	}
	if got.MerkleRootHex != rootHex {
		t.Errorf("old row merkle_root changed: %q != %q (migration must not invalidate)", got.MerkleRootHex, rootHex)
	}
	proof, err := povi.BuildProof(runeSteps, 0)
	if err != nil {
		t.Fatal(err)
	}
	storedRoot, err := hex.DecodeString(got.MerkleRootHex)
	if err != nil || len(storedRoot) != 32 {
		t.Fatalf("stored root not 32 bytes: %v", err)
	}
	var r32 [32]byte
	copy(r32[:], storedRoot)
	if !povi.VerifyPath(r32, runeSteps[0], proof) {
		t.Error("old rune-rooted receipt no longer validates after 0060 — must remain verifiable")
	}
}

// TestRecordReceipt_PersistsLeafKind — rune and token receipts persist their kind and
// count; the stored tag matches what was committed.
func TestRecordReceipt_PersistsLeafKind(t *testing.T) {
	store, _ := leafKindEnv(t)
	ctx := context.Background()

	cases := []struct {
		reqID string
		steps [][]byte
		kind  povi.LeafKind
	}{
		{"rune-req", povi.StepsFromRunes("héllo"), povi.LeafKindRune},
		{"tok-req", povi.StepsFromTokens([]string{"hé", "llo", " wörld"}), povi.LeafKindToken},
	}
	for _, c := range cases {
		rec := povi.Receipt{
			RequestID: c.reqID, NodeID: "n", WorkspaceID: "ws", Model: "m",
			InputTokens: 1, OutputTokens: len(c.steps),
			MerkleRoot: povi.MerkleRoot(c.steps),
			Timestamp:  100, LeafCount: len(c.steps), LeafKind: c.kind,
		}
		ins, err := store.RecordReceipt(ctx, rec, true)
		if err != nil || !ins {
			t.Fatalf("RecordReceipt(%s): inserted=%v err=%v", c.reqID, ins, err)
		}
		got, err := store.GetReceipt(ctx, c.reqID)
		if err != nil {
			t.Fatalf("GetReceipt(%s): %v", c.reqID, err)
		}
		if got.LeafKind != string(c.kind) {
			t.Errorf("%s persisted leaf_kind = %q, want %q", c.reqID, got.LeafKind, c.kind)
		}
		if got.LeafCount != len(c.steps) {
			t.Errorf("%s leaf_count = %d, want %d", c.reqID, got.LeafCount, len(c.steps))
		}
	}
}
