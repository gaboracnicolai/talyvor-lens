-- B30.1 — a clearance names a licence, a partner, the countries it covers and when it ends.
--
-- A capability takes live money only while its latest wallet_clearances row (0158) is a 'clear' that has not
-- passed expires_at, and only for a use from a country the clearance lists (ISO 3166-1 alpha-2). A clearance past
-- its expiry, or used from a country it does not list, refuses live money exactly like no clearance.
--
-- A clear recorded before this migration names none of these, so it lists no country and has no expiry: it lets
-- no live money through, and the operator records it again with its terms.
ALTER TABLE wallet_clearances
  ADD COLUMN IF NOT EXISTS countries         TEXT[] NOT NULL DEFAULT '{}',  -- ISO 3166-1 alpha-2, where live money may be used from
  ADD COLUMN IF NOT EXISTS partner           TEXT NOT NULL DEFAULT '',      -- the licensed partner the money moves through
  ADD COLUMN IF NOT EXISTS licence_reference TEXT NOT NULL DEFAULT '',      -- the licence it rests on
  ADD COLUMN IF NOT EXISTS expires_at        TIMESTAMPTZ;                   -- when it stops; NULL only on a revoke or an old clear

-- Every clear from now on names all four. NOT VALID: the clears recorded before this migration keep their rows
-- (the table is append-only) and, naming no country, clear nothing.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'wallet_clearances_clear_terms') THEN
    ALTER TABLE wallet_clearances ADD CONSTRAINT wallet_clearances_clear_terms CHECK (
      action <> 'clear' OR (
        cardinality(countries) > 0
        AND array_to_string(countries, ',') ~ '^[A-Z]{2}(,[A-Z]{2})*$'
        AND partner <> ''
        AND licence_reference <> ''
        AND expires_at IS NOT NULL
      )
    ) NOT VALID;
  END IF;
END $$;
