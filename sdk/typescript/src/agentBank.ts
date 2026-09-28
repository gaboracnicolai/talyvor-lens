/**
 * AgentBank — an agent's own account at Talyvor Lens.
 *
 * An agent calls these with ITS OWN key (a key attached to the agent): its
 * balance and spending rules, an approval asked for with a reason, a payment
 * to another agent of its workspace, and the receipt of an entry it is party
 * to. The agent is the key's, never an argument, so a key reaches only its
 * own account; every call — served or refused — is logged by Lens.
 *
 * They are Lens's MCP tools (agent_balance, agent_request_approval,
 * agent_pay, agent_receipt) called over JSON-RPC at ``/mcp``, so an agent
 * using the SDK and an agent using MCP are judged by the same rules.
 */

import type {
  AgentApproval,
  AgentBalance,
  AgentPayment,
  AgentReceipt,
} from "./types";

/** Lens could not answer the call (network, credential, protocol). */
export class AgentBankError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AgentBankError";
  }
}

/**
 * The bank refused what the agent asked, and says why — for example the
 * payment needs an approval that is still pending, it is beyond the agent's
 * limit per request, or the agent is paused.
 */
export class PaymentRefused extends AgentBankError {
  constructor(message: string) {
    super(message);
    this.name = "PaymentRefused";
  }
}

/**
 * The agent bank tools, called with the client's key. Reach it as
 * ``new LensClient({...}).bank``; the key must be attached to an agent.
 * Amounts are in µLXC (1 LXC = 1,000,000 µLXC).
 *
 * Usage:
 *   const bank = new LensClient({ lensUrl, apiKey: agentKey }).bank;
 *   await bank.requestApproval("agt_seller", 2_000_000, "October hosting");
 *   // ...once a person approves it on the Agent Bank screen:
 *   const payment = await bank.pay("agt_seller", 2_000_000);
 *   const receipt = await bank.receipt(payment.entry_id);
 */
export class AgentBank {
  private readonly url: string;
  private readonly headers: Record<string, string>;
  private nextId = 1;

  constructor(lensUrl: string, headers: Record<string, string>) {
    this.url = `${lensUrl}/mcp`;
    this.headers = { ...headers, "Content-Type": "application/json" };
  }

  /** The agent's account: balance, spent, paused, and its spending rules. */
  balance(): Promise<AgentBalance> {
    return this.call("agent_balance", {});
  }

  /**
   * Ask a person to approve a payment above the agent's approval amount.
   * Once approved, make exactly that payment — same recipient, amount and
   * memo — with ``pay``. Asking again for the same payment returns the
   * approval already open, with the new reason.
   */
  requestApproval(
    toAgentId: string,
    amountUlxc: number,
    reason: string,
    memo = "",
  ): Promise<AgentApproval> {
    return this.call("agent_request_approval", {
      to_agent_id: toAgentId,
      amount_ulxc: amountUlxc,
      memo,
      reason,
    });
  }

  /**
   * Pay another agent of the workspace from the agent's own balance.
   * Rejects with ``PaymentRefused`` when the agent's rules refuse it.
   */
  pay(toAgentId: string, amountUlxc: number, memo = ""): Promise<AgentPayment> {
    return this.call("agent_pay", {
      to_agent_id: toAgentId,
      amount_ulxc: amountUlxc,
      memo,
    });
  }

  /** Every posting of an entry on the agent's account; they sum to zero. */
  receipt(entryId: string): Promise<AgentReceipt> {
    return this.call("agent_receipt", { entry_id: entryId });
  }

  private async call<T>(tool: string, args: Record<string, unknown>): Promise<T> {
    const body = JSON.stringify({
      jsonrpc: "2.0",
      id: this.nextId++,
      method: "tools/call",
      params: { name: tool, arguments: args },
    });
    let resp: Response;
    try {
      resp = await fetch(this.url, { method: "POST", headers: this.headers, body });
    } catch (err) {
      throw new AgentBankError(`${tool}: ${(err as Error).message}`);
    }
    const text = await resp.text();
    if (resp.status !== 200) {
      throw new AgentBankError(`${tool}: Lens answered ${resp.status}: ${text.trim()}`);
    }
    let envelope: {
      error?: { message?: string };
      result?: { content?: { text?: string }[]; isError?: boolean };
    };
    try {
      envelope = JSON.parse(text);
    } catch {
      throw new AgentBankError(`${tool}: Lens answered something other than JSON-RPC`);
    }
    if (envelope.error) {
      throw new AgentBankError(`${tool}: ${envelope.error.message ?? JSON.stringify(envelope.error)}`);
    }
    const out = envelope.result?.content?.[0]?.text ?? "";
    if (envelope.result?.isError) {
      throw new PaymentRefused(out);
    }
    try {
      return JSON.parse(out) as T;
    } catch {
      throw new AgentBankError(`${tool}: unreadable result: ${out}`);
    }
  }
}
