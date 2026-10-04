/**
 * Agents — the workspace owner's side of Agent Wallets at Talyvor Lens.
 *
 * Called with the owner's key (a workspace key with the keys or admin scope):
 * create an agent, fund its wallet from the workspace's LXC, issue the agent
 * a key of its own, and read its statement. The agent then calls through Lens
 * with that key, and every model call is charged to its wallet, within its
 * spending rules, before the provider is called.
 *
 * They are Lens's REST routes under ``/v1/workspaces/{ws}/agents``, in the
 * client's workspace. A non-2xx answer rejects with ``AgentWalletError``
 * carrying Lens's status and message.
 */

import { AgentWalletError } from "./agentWallet";
import type { AgentAccount, AgentKey, AgentStatement } from "./types";

/**
 * Reach it as ``new LensClient({...}).agents``. Amounts are in µLXC
 * (1 LXC = 1,000,000 µLXC).
 *
 * Usage:
 *   const agents = new LensClient({ lensUrl, apiKey: ownerKey, workspaceId: "acme" }).agents;
 *   const agent = await agents.create("researcher");
 *   await agents.fund(agent.id, 10_000_000);
 *   const { key } = await agents.issueKey(agent.id);
 *   // ...the agent calls through Lens with `key`, then:
 *   const { lines } = await agents.statement(agent.id);
 */
export class Agents {
  private readonly base: string;
  private readonly headers: Record<string, string>;

  constructor(lensUrl: string, workspaceId: string, headers: Record<string, string>) {
    this.base = `${lensUrl}/v1/workspaces/${encodeURIComponent(workspaceId)}/agents`;
    this.headers = { ...headers, "Content-Type": "application/json" };
  }

  /** Every agent in the workspace, with its balance. */
  async list(): Promise<AgentAccount[]> {
    const book = await this.send<{ agents: AgentAccount[] }>("GET", "");
    return book.agents;
  }

  /** Create an agent. It starts with an empty wallet and no key. */
  create(name: string): Promise<AgentAccount> {
    return this.send("POST", "", { name });
  }

  /**
   * Move µLXC from the workspace to the agent's wallet. Resolves to its new
   * balance. Sent again with the same ``idempotencyKey``, the move lands once.
   */
  fund(agentId: string, amountUlxc: number, opts: { idempotencyKey?: string } = {}): Promise<{ agent_id: string; balance_ulxc: number }> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/fund`, { amount_ulxc: amountUlxc }, opts.idempotencyKey);
  }

  /** Take µLXC back from the agent's wallet to the workspace. */
  withdraw(agentId: string, amountUlxc: number, opts: { idempotencyKey?: string } = {}): Promise<{ agent_id: string; balance_ulxc: number }> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/withdraw`, { amount_ulxc: amountUlxc }, opts.idempotencyKey);
  }

  /**
   * Issue the agent a proxy key of its own. ``key`` is shown once: a client
   * built with it calls models on the agent's wallet and reaches ``.wallet``.
   */
  issueKey(agentId: string, name = ""): Promise<AgentKey> {
    return this.send("POST", `/${encodeURIComponent(agentId)}/keys`, name ? { name } : {});
  }

  /** The agent's account, newest first: funding, model spend, payments (at most 1000 lines). */
  statement(agentId: string, opts: { limit?: number } = {}): Promise<AgentStatement> {
    const q = opts.limit ? `?limit=${opts.limit}` : "";
    return this.send("GET", `/${encodeURIComponent(agentId)}/statement${q}`);
  }

  private async send<T>(method: string, path: string, body?: unknown, idempotencyKey?: string): Promise<T> {
    const headers = { ...this.headers };
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    const what = `${method} ${this.base}${path}`;
    let resp: Response;
    try {
      resp = await fetch(`${this.base}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch (err) {
      throw new AgentWalletError(`${what}: ${(err as Error).message}`);
    }
    const text = await resp.text();
    if (resp.status < 200 || resp.status >= 300) {
      throw new AgentWalletError(`${what}: Lens answered ${resp.status}: ${text.trim()}`);
    }
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new AgentWalletError(`${what}: unreadable answer: ${text}`);
    }
  }
}
