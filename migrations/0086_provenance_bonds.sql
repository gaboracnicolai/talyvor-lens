-- 0086_provenance_bonds.sql
-- H5.β PROVENANCE BONDS — the money path. A workspace STAKES collateral on a specific gateway-bound
-- output_id (a code generation it relies on). If the mechanical verdict for that output comes back a
-- self-reported FAILURE (compile_failed / tests_failed — the ONLY slash-usable signal, see
-- outputverify.IsSlashUsable), the bond is SLASHED: the collateral is BURNED via mining.SlashStake (supply
-- reduced, paid to NOBODY — no counterparty, no treasury). If no slash-usable verdict exists by the appeal
-- deadline, the bond RELEASES (collateral returned). TIME releases a bond, never a self-attestation: a
-- self-reported PASS proves nothing and NEVER releases it early.
--
-- WHY NOT povi_stakes: that table is node_id-PK'd NODE collateral — semantically wrong for a workspace ×
-- output bond. This is a thin, purpose-built table; the actual value movement reuses the EXISTING
-- mining.LockStake/SlashStake/ReleaseStake ledger (no new mint type, no new ledger).
--
-- MONEY DISCIPLINE: amount_ulens is BIGINT µLENS (1 LENS = 1e6 µLENS) — INTEGER, never float (SEC-2). SELF
-- workspace only (no counterparty column). Append-only status transitions via CAS (WHERE status=...). The
-- bond is bound to an output the workspace OWNS (enforced at create: output_id ∈ k4_output_verdicts with
-- this workspace_id) — so only the bonder's OWN self-reported failure can ever slash it (workspace B can
-- neither report a verdict on A's output nor slash A's bond).
CREATE TABLE IF NOT EXISTS provenance_bonds (
    bond_id         TEXT PRIMARY KEY,                                 -- server-derived: sha256("h5_bond:"‖ws‖output_id)
    workspace_id    TEXT NOT NULL,                                    -- SELF only — the bonder (owns output_id)
    output_id       TEXT NOT NULL,                                    -- the gateway-bound identity being bonded (0084)
    amount_ulens    BIGINT NOT NULL CHECK (amount_ulens > 0),         -- µLENS, integer — never float
    slash_bps       INTEGER NOT NULL DEFAULT 10000 CHECK (slash_bps > 0 AND slash_bps <= 10000), -- fraction slashed (bps)
    status          TEXT NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'appealing', 'slashed', 'released')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    appeal_deadline TIMESTAMPTZ NOT NULL,                             -- burn finalizes only at/after this; window is contestable
    settled_at      TIMESTAMPTZ,                                      -- when slashed or released
    slash_key       TEXT UNIQUE                                       -- server-derived, set on slash; UNIQUE dedup (no replay)
);

CREATE INDEX IF NOT EXISTS idx_provenance_bonds_ws ON provenance_bonds (workspace_id, status);
CREATE INDEX IF NOT EXISTS idx_provenance_bonds_settle ON provenance_bonds (appeal_deadline) WHERE status IN ('active', 'appealing');
