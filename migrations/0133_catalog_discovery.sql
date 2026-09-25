-- 0133_catalog_discovery.sql — B10.5: new models appear on their own.
--
-- catalog_provider_lists is the latest SUCCESSFUL model list of each provider Lens polls
-- (internal/modelwatch): a catalog model its provider no longer lists is marked retired and leaves
-- the chat picker. catalog_discovered_models is every listed id the catalog could not price, from
-- the day it was first seen; input/output stay NULL — "needs a price", never billed as free — until a
-- person confirms the provider's published rate (PUT /v1/admin/catalog/models/{id}/price, with the
-- pricing page's URL), which puts the model in the catalog and the picker.

CREATE TABLE IF NOT EXISTS catalog_provider_lists (
    provider   TEXT        PRIMARY KEY,
    model_ids  TEXT[]      NOT NULL,
    listed_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS catalog_discovered_models (
    provider       TEXT             NOT NULL,
    model_id       TEXT             NOT NULL,
    first_seen_at  TIMESTAMPTZ      NOT NULL,
    input_per_1m   DOUBLE PRECISION CHECK (input_per_1m > 0),
    output_per_1m  DOUBLE PRECISION CHECK (output_per_1m >= 0),
    price_source   TEXT,
    priced_at      TIMESTAMPTZ,
    PRIMARY KEY (provider, model_id),
    CHECK ((input_per_1m IS NULL) = (output_per_1m IS NULL) AND (input_per_1m IS NULL) = (price_source IS NULL))
);
