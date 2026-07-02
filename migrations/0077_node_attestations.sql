-- 0077_node_attestations.sql — Proof-of-Confidential-Compute step (b): the gateway-VERIFIED attestation
-- record. A node proves it runs on genuine NVIDIA confidential-compute hardware via an NRAS-signed EAT the
-- GATEWAY cryptographically verifies (JWT sig + x5c chain to the pinned NVIDIA root CA + claim checks) — NOT
-- node-asserted. This table also DOUBLES as the single-use nonce store: a row is INSERTed 'pending' at
-- challenge-issue time and atomically CAS-consumed at verify (nonce PK = single-use; node_id = binding).
--
-- gpu_type (inference_nodes) STAYS node-declared/untrusted (routing/display). attested_gpu_class here is the
-- cryptographically-verified truth. Additive: a NEW table only — no ALTER/DROP on inference_nodes, any
-- ledger, or any economy table. Mints nothing (step c is the mint).
--
-- key_bound is the RELAY-GAP FENCE. The NVIDIA EAT attests the GPU, not the node's ed25519 identity — so a
-- node A with no CC GPU could relay a real CC node B's EAT under A's own nonce, wrapped with A's key. That
-- is only fully closed by an ENCLAVE-BOUND signing key (the node's key generated inside the CC boundary and
-- measured in the EAT's report_data). Until NVIDIA report_data key-binding is available, EVERY verified row
-- records key_bound=false. When it is, verify adds check (iv) EAT.report_data == H(registered pubkey) →
-- key_bound=true. **STEP (c) WILL PAY ONLY ON attestation_status='verified' AND key_bound=true AND
-- expires_at > now()** — so relay-vulnerable (key_bound=false) rows structurally cannot reach the mint.
CREATE TABLE IF NOT EXISTS node_attestations (
    nonce              BIGINT       PRIMARY KEY,                 -- crypto/rand challenge — SINGLE-USE key
    node_id            TEXT         NOT NULL,                    -- the node the nonce was issued to (binding)
    attestation_status TEXT         NOT NULL DEFAULT 'pending',  -- pending → verified | failed
    attested_gpu_class TEXT,                                     -- NULL until verified; the VERIFIED class the mint reads
    cc_mode            BOOLEAN,                                  -- NULL until verified; confidential-computing on
    eat_digest         TEXT,                                     -- sha256 hex of the verified EAT (audit; non-reversible)
    key_bound          BOOLEAN      NOT NULL DEFAULT false,      -- true ONLY when EAT.report_data == H(node pubkey) verified
    attested_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),      -- issue time (also the challenge-window anchor)
    expires_at         TIMESTAMPTZ                               -- attestation validity end (set on verify)
);

-- Backs the "latest attestation per node" read (step c's mint gate + operator status).
CREATE INDEX IF NOT EXISTS idx_node_attestations_node_recent
    ON node_attestations (node_id, attested_at DESC);
