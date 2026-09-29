# Pots (B22.7)

An agent can keep credits aside in named pots:
- a **goal**, with a target;
- a **budget**;
- a **reserve**.

It moves credits between a pot and its main balance, and can lock a pot until a date. Pots pay no interest,
because interest is class RED. Pots themselves are GREEN.

```
GET  /v1/workspaces/{ws}/agents/{agent}/pots                  each pot and what it holds
POST /v1/workspaces/{ws}/agents/{agent}/pots                  {"name": "new laptop", "kind": "goal", "target_ulxc": 50000000, "locked_until": "2026-12-01T00:00:00Z"}
POST /v1/workspaces/{ws}/agents/{agent}/pots/{pot}/in         {"amount_ulxc": 30000000}   from the main balance
POST /v1/workspaces/{ws}/agents/{agent}/pots/{pot}/out        {"amount_ulxc": 10000000}   back, unless locked
PUT  /v1/workspaces/{ws}/agents/{agent}/pots/{pot}/lock       {"locked_until": "…"} or {"locked_until": null}
```

The agent's own key, the workspace's owner or an admin may do each of these.

## What a pot's credits are

Each move is one `agent_postings` entry, `agent:<id>` ↔ `pot:<pot id>`, of kind `pot_in` or `pot_out`.

A pot's credits are still the agent's:
- they count with what the workspace's agents hold, so the workspace cannot spend them;
- the agent spends only its main balance.

The Agent Wallets book shows each agent's `pots_ulxc`. An agent's statement lists each pot and what it held at
the end of the period, and each move appears as a line.

A locked pot refuses to give anything back before its date, and a lock still in force can only be made
longer — neither the agent nor its owner can lift it early. Once its date has passed it can be set again or
removed.
