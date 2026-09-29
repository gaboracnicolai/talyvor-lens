# Talyvor Lens Python SDK

Drop-in OpenAI / Anthropic client that routes every request through Talyvor Lens — caching, routing, attribution, cost tracking — without changing your application code.

## Installation

```bash
pip install talyvor-lens
# Optional: add anthropic support
pip install "talyvor-lens[anthropic]"
```

Requires Python 3.10+.

## Quick start (3 lines)

```python
from talyvor_lens import LensClient
client = LensClient(lens_url="http://your-lens:8080", api_key="tlv_...")
response = client.openai.chat.completions.create(model="gpt-4o", messages=[{"role":"user","content":"Hello"}])
```

That's it — the request flows through Lens, gets cached, routed, and recorded against the default workspace.

## With session tracking

```python
turn = client.set_session("sess-abc123", agent_name="researcher")
reply = turn.openai.chat.completions.create(...)
```

Each call under the same `session_id` is grouped into one conversation in the Lens dashboard.

## With Git branch attribution

```python
pr_client = client.set_branch("feat/new-login", pr_number="142")
result = pr_client.openai.chat.completions.create(...)
```

Spend incurred on this PR shows up in `/v1/api/attribution/branch`.

## Standalone header injection

If you already have an HTTP client (httpx, requests, aiohttp), use `inject_lens_headers`:

```python
import httpx
from talyvor_lens import inject_lens_headers

headers = inject_lens_headers(
    api_key="tlv_...",
    workspace_id="finance",
    session_id="sess-1",
    team="ml-platform",
)
response = httpx.post("http://lens:8080/v1/proxy/openai/chat/completions", headers=headers, json=body)
```

## The agent wallet

With a key attached to an agent (`POST /v1/workspaces/{ws}/agents/{agent}/keys`), the agent
uses its own account. Amounts are µLXC (1 LXC = 1,000,000 µLXC); every call is
logged by Lens, and the agent's spending rules judge every payment.

```python
from talyvor_lens import LensClient, PaymentRefused

wallet = LensClient(lens_url="https://lens.talyvor.com", api_key="tlv_ws_...").wallet

wallet.balance()["agent"]["balance_ulxc"]        # 10000000
approval = wallet.request_approval("agt_seller", 2_000_000, reason="October hosting")
# ...a person approves it on the Agent Wallets screen, then:
payment = wallet.pay("agt_seller", 2_000_000)
wallet.receipt(payment["entry_id"])["postings"]  # every posting, summing to zero

try:
    wallet.pay("agt_seller", 6_000_000)
except PaymentRefused as refusal:
    print(refusal)  # "...the agent's limit per request is 5 LXC"
```

A refusal by Lens is `PaymentRefused`; Lens not answering (network, a
rejected key) is its parent, `AgentWalletError`.

### Everything else the wallet does

The same key reaches every capability of the wallet (Lens's `wallet_*` MCP tools). Each is
judged by the agent's rules and by its capability's class: a refusal says which.

```python
wallet.send("@seller", 1_000_000, memo="hosting")          # to any agent on Talyvor
wallet.request("@buyer", 2_000_000)                         # it accepts or declines
wallet.answer_request("mreq_…", accept=True)                # pay a request made of the agent
wallet.refund("xfer_…")                                     # give a received transfer back
wallet.credit_line()                                        # the company's line: limit, used, available
wallet.offer_loan("@other-co", 30_000_000, 3, "week", interest_bps=500)
wallet.answer_loan("loan_…", accept=True)                   # take a loan offered to the agent
wallet.escrow_pay("@seller", 5_000_000, "2026-10-06T12:00:00Z", memo="logo")
wallet.escrow_confirm("escrow_…")                           # or escrow_dispute(id, reason)
pot = wallet.pot_create("rainy day", "reserve", locked_until="2026-12-01T00:00:00Z")
wallet.pot_move(pot["id"], 1_000_000, "in")
pf = wallet.portfolio_open("fx", 1_000_000_000)             # simulated USD: no money moves
wallet.order(pf["id"], "EUR", "buy", "limit", 100_000_000, limit_price_usd="1.15")
```

Each has a reader too: `requests()`, `loans()`, `escrows()`, `pots()`, `quotes()` and
`portfolios()`.

## Headers set by the SDK

| Header | Source | Purpose |
|--------|--------|---------|
| `Authorization: Bearer …` | `api_key` | Lens API key authentication |
| `X-Talyvor-Workspace` | `workspace_id` (default: `"default"`) | Cache + policy scope |
| `X-Talyvor-Team` | `team` | Spend attribution bucket |
| `X-Talyvor-Feature` | `feature` | Spend attribution bucket |
| `X-Talyvor-Session` | `session_id` | Multi-turn agent session ID |
| `X-Talyvor-Agent` | `agent_name` | Agent identifier within a session |
| `X-Talyvor-Branch` | `branch` | Git branch for PR-level cost attribution |
| `X-Talyvor-PR` | `pr_number` | GitHub PR number |
| `X-Talyvor-Commit` | `commit` | Git commit SHA |
| `X-Talyvor-Repository` | `repository` | `owner/name` repo identifier |

Empty values are silently dropped — no blank headers ever leave the SDK.

## License

[MIT](LICENSE). This SDK is deliberately licensed more permissively than the Talyvor Lens server
it talks to, which is under the [Business Source License 1.1](../../LICENSE): the SDK is a thin
client you embed in your own application, so it should not put a licence review in the way of
an integration.
