-- 0093_eval_correctness_attestations.sql — Proof-of-eval-contribution CORRECTNESS CONSENSUS.
--
-- WHY: the eval-contribution mint pays for a DISCRIMINATING eval item, but discrimination alone does not
-- establish that the contributor's CLAIMED expected_output is CORRECT. A self-certified wrong answer that
-- happens to split models could otherwise earn. This table records INDEPENDENT workspaces' judgments that an
-- item's claimed answer is (in)correct; the eval minter withholds payment until enough INDEPENDENT operators
-- agree (see internal/poolroyalty/eval_consensus_gate.go). The submitter's own assertion is never trusted.
--
-- Independence is bound OFF-TABLE by the transitive identity graph (card fingerprint ∪ owner_key, union-find)
-- — the SAME graph the ring detector uses — so a workspace cannot self-consense through its own sockpuppets.
-- This table only stores the raw attestations; the operator-clustering happens in the minter.
--
-- One attestation per (item, attester): PK (item_id, attester_workspace_id) makes re-attestation an upsert of
-- the same operator's single vote, never a way to inflate the count. Mint-free itself (no ledger, no money).
CREATE TABLE IF NOT EXISTS eval_correctness_attestations (
    item_id               TEXT        NOT NULL,               -- benchmark_eval_items.id being attested
    attester_workspace_id TEXT        NOT NULL,               -- the INDEPENDENT workspace casting a correctness judgment
    agrees                BOOLEAN     NOT NULL,               -- true = the claimed answer is correct; false = disputed
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (item_id, attester_workspace_id)              -- one vote per (item, attester); re-attest = upsert
);

-- The consensus read for one item rides this index (SELECT ... WHERE item_id = $1).
CREATE INDEX IF NOT EXISTS idx_eval_correctness_attestations_item
    ON eval_correctness_attestations (item_id);
