# Credit lines for companies (B22.4)

A company can spend now and pay monthly. Talyvor sets the company a credit line. When one of the company's
agents spends on Talyvor services past what it holds, the line lends the difference, up to its limit.

Marketplace listings need no line. They are already billed monthly on the company's marketplace bill and are
never taken from credits.

Only a company can have a credit line. Credit involving a private user is class RED, so a private user's
workspace is refused. For a company the line is GREEN.

## Setting a line (operator)

Run inside the lens container:

```
lens credit-lines                               every line: limit, used, available, paused
lens credit-lines company <workspace> on|off    record whether a workspace is a company
lens credit-lines set <workspace> <limit LXC>   give a company a line, or change its limit
lens credit-lines pause <workspace> <why>       stop the line lending
lens credit-lines resume <workspace>            let it lend again
```

## What the company sees

```
GET /v1/workspaces/{ws}/credit-line
→ {"limit_ulxc": …, "used_ulxc": …, "available_ulxc": …, "paused": false, "paused_reason": "", "invoices": [ … ]}
```

`used_ulxc` is what the line has lent and not yet been paid for.

## How a draw is recorded

When an agent's model call costs more than the agent holds, the shortfall is lent in the same transaction as
the spend:
- an `lxc_ledger` row of type `credit_line_draw` credits the workspace;
- a `credit_line` entry in `agent_postings` moves it to the agent;
- a `credit_line_draws` row records the draw.

The agent's rules still judge the spend. If the spend is refused, nothing is lent.

A draw is refused when:
- it would pass the limit;
- the operator has paused the line;
- an invoice of the line is unpaid past its due date.

The proxy's balance gate counts what the line can still lend for an agent's request.

## Monthly invoice

On the first of each month (UTC), billing puts every draw made before that day on one Stripe invoice. The
invoice is priced at the LXC peg and due in 14 days.

When the invoice is paid (`invoice.paid`), its draws stop counting against the limit. If it is unpaid past its
due date, the line is paused until it is paid. Paying it lets the line lend again with no operator step.
