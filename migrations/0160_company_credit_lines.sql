-- B22.4 — Talyvor's credit line for companies: spend now, pay monthly.
--
-- workspaces.company is the operator's record that a workspace is a company (`lens credit-lines company`).
-- Only a company can have a credit line: credit involving a private user is class RED (B22.1); for a company
-- the line is GREEN.
--
-- When an agent of a company spends on Talyvor services past what it holds, the line lends the shortfall: an
-- lxc_ledger row of type 'credit_line_draw' credits the workspace, a fund entry moves it to the agent, and a
-- credit_line_draws row records it. Each month billing puts the draws of the months before on one Stripe
-- invoice (credit_line_invoices); once it is paid its draws stop counting against the limit. An invoice
-- unpaid past its due date pauses the line until it is paid; the operator can also pause it (paused_at).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS company BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS company_credit_lines (
    workspace_id  TEXT PRIMARY KEY,
    limit_ulxc    BIGINT NOT NULL CHECK (limit_ulxc > 0),
    paused_at     TIMESTAMPTZ,                                 -- the operator paused it
    paused_reason TEXT NOT NULL DEFAULT '',
    set_by        TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credit_line_invoices (
    id                TEXT PRIMARY KEY,                        -- cli_<uuid>
    workspace_id      TEXT NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,                    -- it carries the draws made before this
    amount_ulxc       BIGINT NOT NULL CHECK (amount_ulxc > 0),
    amount_cents      BIGINT NOT NULL CHECK (amount_cents > 0),
    stripe_invoice_id TEXT NOT NULL UNIQUE,
    due_at            TIMESTAMPTZ NOT NULL,
    paid_at           TIMESTAMPTZ,                             -- invoice.paid; unpaid past due_at pauses the line
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, period_end)
);

CREATE TABLE IF NOT EXISTS credit_line_draws (
    id           TEXT PRIMARY KEY,                             -- cld_<uuid>
    workspace_id TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    amount_ulxc  BIGINT NOT NULL CHECK (amount_ulxc > 0),
    ref          TEXT NOT NULL DEFAULT '',                     -- the request or reservation it paid for
    invoice_id   TEXT NOT NULL DEFAULT '',                     -- the credit_line_invoices row it went on
    drawn_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credit_line_draws_ws ON credit_line_draws (workspace_id, invoice_id, drawn_at);
