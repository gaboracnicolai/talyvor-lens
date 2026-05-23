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

Apache-2.0
