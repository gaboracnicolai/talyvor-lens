-- B32.22 — agents buy, rent and subscribe only within their mandate.
--
-- Three rules bound the marketplace licences an agent may take, judged with its other rules in the one judge
-- (enforceAgentRules), before the licence's billed row is written:
--   max_commitment_ulxc — the most one licence may commit it to: a buy's or a rent's price, one subscription period's.
--                         NULL is no limit, as for the other limits.
--   allowed_licences    — the licences it may hold: commercial and enterprise. Empty allows both; personal is never an
--                         agent's, so it cannot be named.
--   may_subscribe       — whether it may subscribe, or take a rent that renews itself; it may not unless its owner
--                         says so. A renewal needs it too, so turning it off stops the agent's licences renewing.
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS max_commitment_ulxc BIGINT CHECK (max_commitment_ulxc > 0);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS allowed_licences TEXT[] NOT NULL DEFAULT '{}'
    CHECK (allowed_licences <@ ARRAY['commercial', 'enterprise']::text[]);
ALTER TABLE agent_rules ADD COLUMN IF NOT EXISTS may_subscribe BOOLEAN NOT NULL DEFAULT false;

-- An agent already holding a licence that renews keeps it renewing: the licence was taken before this rule.
INSERT INTO agent_rules (agent_id, workspace_id, may_subscribe)
SELECT DISTINCT c.agent_id, c.buyer_workspace_id, true
  FROM market_licences c JOIN agent_accounts a ON a.id = c.agent_id AND a.workspace_id = c.buyer_workspace_id
 WHERE c.status = 'active' AND c.auto_renew
ON CONFLICT (agent_id) DO UPDATE SET may_subscribe = true;

-- Every version of an agent's rules (0192) names the three as its rules hold them now, so the next save that changes
-- nothing is not a new version, and a rollback to a version taken before them keeps them as they are.
UPDATE agent_rules_versions v
   SET rules = v.rules || jsonb_build_object('max_commitment_ulxc', r.max_commitment_ulxc,
                                             'allowed_licences', to_jsonb(r.allowed_licences), 'may_subscribe', r.may_subscribe)
  FROM agent_rules r
 WHERE r.agent_id = v.agent_id AND NOT v.rules ? 'may_subscribe';
