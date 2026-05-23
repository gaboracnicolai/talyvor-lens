# Talyvor Lens TypeScript SDK

Drop-in OpenAI client wrapper that routes every request through Talyvor Lens — caching, routing, attribution, cost tracking — without changing your application code.

## Installation

```bash
npm install talyvor-lens
# OpenAI is a peer dependency:
npm install openai
```

Requires Node 18+.

## Quick start (3 lines)

```typescript
import { LensClient } from "talyvor-lens";
const client = new LensClient({ lensUrl: "http://your-lens:8080", apiKey: "tlv_..." });
const ai = client.openai();
```

Then use `ai` exactly like the standard OpenAI client:

```typescript
const r = await (ai as any).chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Hello" }],
});
```

## With session tracking

```typescript
const turn = client.withSession("sess-abc123", "researcher");
const reply = await (turn.openai() as any).chat.completions.create({ ... });
```

Each call under the same `sessionId` is grouped into one conversation in the Lens dashboard.

## With Git branch attribution

```typescript
const pr = client.withBranch("feat/new-login", "142");
const result = await (pr.openai() as any).chat.completions.create({ ... });
```

Spend incurred on this PR shows up in `/v1/api/attribution/branch`.

## Standalone header injection

If you already have an HTTP client (fetch, axios, undici), use `injectLensHeaders`:

```typescript
import { injectLensHeaders } from "talyvor-lens";

const headers = injectLensHeaders(
  {},
  {
    apiKey: "tlv_...",
    workspaceId: "finance",
    sessionId: "sess-1",
    team: "ml-platform",
  },
);
await fetch("http://lens:8080/v1/proxy/openai/chat/completions", {
  method: "POST",
  headers,
  body: JSON.stringify(body),
});
```

## Headers set by the SDK

| Header | Source | Purpose |
|--------|--------|---------|
| `Authorization: Bearer …` | `apiKey` | Lens API key authentication |
| `X-Talyvor-Workspace` | `workspaceId` (default: `"default"`) | Cache + policy scope |
| `X-Talyvor-Team` | `team` | Spend attribution bucket |
| `X-Talyvor-Feature` | `feature` | Spend attribution bucket |
| `X-Talyvor-Session` | `sessionId` | Multi-turn agent session ID |
| `X-Talyvor-Agent` | `agentName` | Agent identifier within a session |
| `X-Talyvor-Branch` | `branch` | Git branch for PR-level cost attribution |
| `X-Talyvor-PR` | `prNumber` | GitHub PR number |
| `X-Talyvor-Commit` | `commit` | Git commit SHA |
| `X-Talyvor-Repository` | `repository` | `owner/name` repo identifier |

Empty / undefined values are silently dropped — no blank headers ever leave the SDK.

## License

Apache-2.0
