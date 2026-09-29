-- B18.12 — a discovered model's release date and tier, recorded when discovery first sees it: the date
-- the provider's own model list gives it (Anthropic created_at, OpenAI created), else the day it was
-- first seen; the tier inferred from its id (catalog.TierFor). Both reach the catalog with its price.

ALTER TABLE catalog_discovered_models ADD COLUMN IF NOT EXISTS release_date DATE;
ALTER TABLE catalog_discovered_models ADD COLUMN IF NOT EXISTS tier TEXT
    CHECK (tier IN ('frontier', 'balanced', 'fast', 'embedding'));
