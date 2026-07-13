-- 0088_workspace_owner_links.sql — Phase-0 mint guard (Item A/B).
--
-- OPERATOR-DECLARED owner linkage: which workspaces belong to the SAME human /
-- operator. This is the ONLY linkage signal that covers VOUCHED workspaces.
--
-- WHY THIS EXISTS: the existing owner-linkage guard (pool royalty; now also cache)
-- joins workspace_card_fingerprints — the set of cards a workspace PAID with. A
-- VOUCHED workspace (earn_verified=true, no purchase) has NO captured fingerprint,
-- so that guard is BLIND exactly in the closed-test vouch scenario. There is no
-- automatic server-side signal binding two vouched workspaces to one human (no
-- users table, no created_by/owner on workspaces, no vouch audit, no IP capture —
-- see the Item A recon). The operator, however, KNOWS the mapping (they vouched
-- both). This table makes that out-of-band knowledge explicit and server-side.
--
-- SEMANTICS: two workspaces are LINKED if they share ANY owner_key — mirroring the
-- fingerprint join (share a hash → share an owner_key). The operator populates it
-- when vouching:  INSERT ('ws-A','human-1'), ('ws-B','human-1')  ⇒ A and B linked.
--
-- RESIDUAL GAP (reported, not hidden): a linkage this table can express exists
-- ONLY if the operator declares it. Undeclared vouched workspaces remain unlinked;
-- the rate cap still bounds their yield. This is an operator-discipline knob, not
-- an automatic detector.
CREATE TABLE IF NOT EXISTS workspace_owner_links (
    workspace_id TEXT        NOT NULL,
    owner_key    TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workspace_id, owner_key)
);

-- Serves the linkage self-join on owner_key (mirrors idx_card_fingerprint).
CREATE INDEX IF NOT EXISTS idx_workspace_owner_links_key ON workspace_owner_links (owner_key);
