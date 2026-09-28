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

## The agent bank

With a key attached to an agent (`POST /v1/workspaces/{ws}/agents/{agent}/keys`), the agent
uses its own account. Amounts are µLXC (1 LXC = 1,000,000 µLXC); every call is
logged by Lens, and the agent's spending rules judge every payment.

```python
from talyvor_lens import LensClient, PaymentRefused

bank = LensClient(lens_url="https://lens.talyvor.com", api_key="tlv_ws_...").bank

bank.balance()["agent"]["balance_ulxc"]        # 10000000
approval = bank.request_approval("agt_seller", 2_000_000, reason="October hosting")
# ...a person approves it on the Agent Bank screen, then:
payment = bank.pay("agt_seller", 2_000_000)
bank.receipt(payment["entry_id"])["postings"]  # every posting, summing to zero

try:
    bank.pay("agt_seller", 6_000_000)
except PaymentRefused as refusal:
    print(refusal)  # "...the agent's limit per request is 5 LXC"
```

A refusal by the bank is `PaymentRefused`; Lens not answering (network, a
rejected key) is its parent, `AgentBankError`.

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
