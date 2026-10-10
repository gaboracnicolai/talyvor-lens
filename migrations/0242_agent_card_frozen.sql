-- B17.111 (B28.97) — an agent's card, frozen by its owner.
--
-- While a card is frozen every purchase on it is declined and nothing leaves the agent; unfrozen, purchases are
-- judged by the agent's rules and balance again. A card is issued unfrozen.

ALTER TABLE agent_cards ADD COLUMN IF NOT EXISTS frozen BOOLEAN NOT NULL DEFAULT false;
