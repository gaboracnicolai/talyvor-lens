-- 0031_povi_receipts.sql — PoVI signed-receipt + Merkle-root attestation layer
-- (Token Economy Phase 1, Part 1).
--
-- Two changes:
--
--   1. inference_nodes.ed25519_pubkey — the node's registered ed25519 PUBLIC
--      key (base64). A node generates a keypair, registers the public key here,
--      and signs each receipt with the matching private key; Lens verifies the
--      receipt signature against this column. Coexists with node_secret_hash
--      (the shared-secret request auth is unchanged); the pubkey is an
--      ADDITIONAL attestation key, not a replacement.
--
--      SCOPE: receipts flow ONLY on the compute-mining / inference path (a node
--      serves a generation request and commits to its token trace). cache_nodes
--      and embedding_nodes do NOT get a pubkey — a cached value or an embedding
--      is a different proof problem (not a generation trace), so a receipt is
--      meaningless for them. They are intentionally out of scope for receipts.
--
--   2. povi_receipts — the audit trail: every receipt Lens receives is recorded
--      with its verification outcome, INDEPENDENT of whether any LENS was
--      minted. This is the source of truth for "what each node attested to".
--
-- ATTESTATION, NOT PROOF OF HONEST COMPUTATION: a verified receipt proves a
-- node signed it (attestation) and that no signed field / committed trace was
-- altered afterward (tamper-evidence). It does NOT prove the computation was
-- honest — a node can sign a fabricated trace. Catching that needs Part 3
-- (challenge-and-slash). Minting from receipts is gated OFF by default
-- (LENS_POVI_MINTING_ENABLED) accordingly.

ALTER TABLE inference_nodes ADD COLUMN IF NOT EXISTS ed25519_pubkey TEXT;

CREATE TABLE IF NOT EXISTS povi_receipts (
    request_id    TEXT PRIMARY KEY,
    node_id       TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    model         TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    merkle_root   TEXT NOT NULL,            -- hex-encoded 32-byte root
    verified      BOOLEAN NOT NULL,         -- signature verified against the node's pubkey
    timestamp     BIGINT NOT NULL,          -- node-reported unix seconds (from the signed receipt)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_povi_receipts_workspace
    ON povi_receipts(workspace_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_povi_receipts_verified
    ON povi_receipts(verified);

CREATE INDEX IF NOT EXISTS idx_povi_receipts_node
    ON povi_receipts(node_id);
