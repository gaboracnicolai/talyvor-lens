-- 0099_served_request_measurements.sql — the GATEWAY MEASUREMENT record that the
-- PoVI receipt→LENS mint is priced on (the "gateway-bound request_id" gate).
--
-- WHY: a provisional receipt mint used to be priced on receipt.output_tokens — a
-- NODE-ASSERTED field, signed by the very node that benefits and cross-checked by
-- nothing. A node could commit a tiny honest trace, claim any output-token count,
-- pass every challenge, and over-mint (the mint basis and the challenged quantity
-- were different, unrelated numbers). This table is Lens's OWN record of what it
-- served: for each request the gateway auto-routes to a registered node, it writes
-- (request_id → node_id it dispatched to + Lens-measured output tokens). The mint
-- (povi.MintFromReceipt) resolves the receipt's request_id here, requires that the
-- receipt's node_id MATCHES the node Lens dispatched to (a node can't name another
-- node's request), and prices STRICTLY on output_tokens FROM THIS ROW — never the
-- claim. NO ROW ⇒ NO MINT (fail closed): a receipt for work Lens never served
-- mints nothing.
--
-- request_id is the gateway-assigned X-Request-ID an honest node echoes into its
-- receipt (proxy.tryNodeRouting sets it; the node-auth token already binds
-- {node_id, request_id, body_sha256}). PK (request_id) = one measurement per
-- request; the writer is ON CONFLICT DO NOTHING so the FIRST measurement stands
-- and a retry can't rewrite the mint basis. output_tokens is Lens's own count of
-- the served response (the same len/4 approximation billing uses), so it is
-- bounded by what the node actually produced and Lens actually forwarded.
CREATE TABLE IF NOT EXISTS served_request_measurements (
    request_id    TEXT        PRIMARY KEY,               -- gateway X-Request-ID the node echoes into its receipt
    node_id       TEXT        NOT NULL,                  -- the node Lens dispatched THIS request to (the binding)
    workspace_id  TEXT        NOT NULL,                  -- requester workspace (audit)
    output_tokens INTEGER     NOT NULL,                  -- Lens's OWN measured served output tokens — the mint basis
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
