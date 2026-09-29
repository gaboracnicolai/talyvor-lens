/**
 * AgentWallet — an agent's own wallet at Talyvor Lens.
 *
 * An agent calls these with ITS OWN key (a key attached to the agent): its
 * balance and spending rules, an approval asked for with a reason, a payment
 * to another agent of its workspace, and the receipt of an entry it is party
 * to. The agent is the key's, never an argument, so a key reaches only its
 * own account; every call — served or refused — is logged by Lens.
 *
 * They are Lens's MCP tools (agent_balance, agent_request_approval,
 * agent_pay, agent_receipt, and B22.11's wallet_* tools) called over JSON-RPC
 * at ``/mcp``, so an agent using the SDK and an agent using MCP are judged by
 * the same rules. The wallet_* methods reach every capability of the wallet:
 * send and request credits, give a transfer back, the company's credit line,
 * loans, escrow, pots, and simulated portfolios and orders. Each resolves to
 * Lens's JSON; a refusal — the agent's rules, or a capability's class —
 * rejects with ``PaymentRefused`` saying why.
 */

import type {
  AgentApproval,
  AgentBalance,
  AgentPayment,
  AgentReceipt,
} from "./types";

/** Lens's JSON answer to a wallet_* call. */
export type Json = Record<string, unknown>;

/** Lens could not answer the call (network, credential, protocol). */
export class AgentWalletError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AgentWalletError";
  }
}

/**
 * Lens refused what the agent asked, and says why — for example the
 * payment needs an approval that is still pending, it is beyond the agent's
 * limit per request, or the agent is paused.
 */
export class PaymentRefused extends AgentWalletError {
  constructor(message: string) {
    super(message);
    this.name = "PaymentRefused";
  }
}

/**
 * The agent wallet tools, called with the client's key. Reach it as
 * ``new LensClient({...}).wallet``; the key must be attached to an agent.
 * Amounts are in µLXC (1 LXC = 1,000,000 µLXC).
 *
 * Usage:
 *   const wallet = new LensClient({ lensUrl, apiKey: agentKey }).wallet;
 *   await wallet.requestApproval("agt_seller", 2_000_000, "October hosting");
 *   // ...once a person approves it on the Agent Wallets screen:
 *   const payment = await wallet.pay("agt_seller", 2_000_000);
 *   const receipt = await wallet.receipt(payment.entry_id);
 */
export class AgentWallet {
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

  // B22.11 — every capability of the wallet. Addresses are a wallet id or an @handle.

  /** Send credits to any agent's wallet on Talyvor, within the agent's spending rules. */
  send(to: string, amountUlxc: number, memo = ""): Promise<Json> {
    return this.call("wallet_send", { to, amount_ulxc: amountUlxc, memo });
  }

  /** Ask another agent's wallet for credits; it accepts or declines. */
  request(fromWallet: string, amountUlxc: number, memo = ""): Promise<Json> {
    return this.call("wallet_request", { from: fromWallet, amount_ulxc: amountUlxc, memo });
  }

  /** The requests for credits the agent made and was made. */
  requests(): Promise<Json> {
    return this.call("wallet_requests", {});
  }

  /** Accept (pay, within the agent's rules) or decline a request made of the agent. */
  answerRequest(requestId: string, accept: boolean): Promise<Json> {
    return this.call("wallet_answer_request", { request_id: requestId, accept });
  }

  /** Give a transfer the agent received back to its sender, once. */
  refund(transferId: string): Promise<Json> {
    return this.call("wallet_refund", { transfer_id: transferId });
  }

  /** The company's credit line from Talyvor: limit, used and available. */
  creditLine(): Promise<Json> {
    return this.call("wallet_credit_line", {});
  }

  /** Offer another company's agent a loan, repaid in equal instalments every day, week or month. */
  offerLoan(
    to: string,
    principalUlxc: number,
    instalments: number,
    every: "day" | "week" | "month",
    opts: { interestBps?: number; lateFeeUlxc?: number; memo?: string } = {},
  ): Promise<Json> {
    return this.call("wallet_offer_loan", {
      to,
      principal_ulxc: principalUlxc,
      instalments,
      every,
      interest_bps: opts.interestBps ?? 0,
      late_fee_ulxc: opts.lateFeeUlxc ?? 0,
      memo: opts.memo ?? "",
    });
  }

  /** The loans the agent lent and borrowed, with every instalment. */
  loans(): Promise<Json> {
    return this.call("wallet_loans", {});
  }

  /** Accept (the principal is paid to the agent) or decline a loan offered to it. */
  answerLoan(loanId: string, accept: boolean): Promise<Json> {
    return this.call("wallet_answer_loan", { loan_id: loanId, accept });
  }

  /** Pay into escrow for another agent: held until confirmed, or released at releaseAt undisputed. */
  escrowPay(to: string, amountUlxc: number, releaseAt: Date | string, memo = ""): Promise<Json> {
    const at = releaseAt instanceof Date ? releaseAt.toISOString() : releaseAt;
    return this.call("wallet_escrow_pay", { to, amount_ulxc: amountUlxc, release_at: at, memo });
  }

  /** The escrows the agent paid into and is owed, with each state. */
  escrows(): Promise<Json> {
    return this.call("wallet_escrows", {});
  }

  /** Confirm delivery on an escrow the agent paid into: it is released to the payee now. */
  escrowConfirm(escrowId: string): Promise<Json> {
    return this.call("wallet_escrow_confirm", { escrow_id: escrowId });
  }

  /** Dispute an escrow the agent paid into, before its deadline: held until Talyvor's operator decides. */
  escrowDispute(escrowId: string, reason: string): Promise<Json> {
    return this.call("wallet_escrow_dispute", { escrow_id: escrowId, reason });
  }

  /** The agent's pots and what each holds. */
  pots(): Promise<Json> {
    return this.call("wallet_pots", {});
  }

  /** Make a pot — a goal, a budget or a reserve — optionally locked until a date. */
  potCreate(
    name: string,
    kind: "goal" | "budget" | "reserve",
    opts: { targetUlxc?: number; lockedUntil?: Date | string } = {},
  ): Promise<Json> {
    const args: Record<string, unknown> = { name, kind, target_ulxc: opts.targetUlxc ?? 0 };
    if (opts.lockedUntil !== undefined) {
      args.locked_until = opts.lockedUntil instanceof Date ? opts.lockedUntil.toISOString() : opts.lockedUntil;
    }
    return this.call("wallet_pot_create", args);
  }

  /** Move credits into a pot ("in") or back out of it ("out", unless it is locked). */
  potMove(potId: string, amountUlxc: number, direction: "in" | "out"): Promise<Json> {
    return this.call("wallet_pot_move", { pot_id: potId, amount_ulxc: amountUlxc, direction });
  }

  /** The simulated market's instruments and their prices in USD, from the ECB's reference rates. */
  quotes(): Promise<Json> {
    return this.call("wallet_quotes", {});
  }

  /** Open a simulated portfolio with simulated US dollars (µUSD). No money or credits move. */
  portfolioOpen(name: string, cashUusd: number): Promise<Json> {
    return this.call("wallet_portfolio_open", { name, cash_uusd: cashUusd });
  }

  /** The agent's simulated portfolios, valued at the latest quotes, with every order. */
  portfolios(): Promise<Json> {
    return this.call("wallet_portfolios", {});
  }

  /** Place a market or limit order in a simulated portfolio. Nothing is sent to a market. */
  order(
    portfolioId: string,
    instrument: string,
    side: "buy" | "sell",
    type: "market" | "limit",
    quantityMicros: number,
    limitPriceUsd?: string,
  ): Promise<Json> {
    const args: Record<string, unknown> = { portfolio_id: portfolioId, instrument, side, type, quantity_micros: quantityMicros };
    if (limitPriceUsd !== undefined) args.limit_price_usd = limitPriceUsd;
    return this.call("wallet_order", args);
  }

  /** Cancel an open order in a simulated portfolio. */
  orderCancel(portfolioId: string, orderId: string): Promise<Json> {
    return this.call("wallet_order_cancel", { portfolio_id: portfolioId, order_id: orderId });
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
      throw new AgentWalletError(`${tool}: ${(err as Error).message}`);
    }
    const text = await resp.text();
    if (resp.status !== 200) {
      throw new AgentWalletError(`${tool}: Lens answered ${resp.status}: ${text.trim()}`);
    }
    let envelope: {
      error?: { message?: string };
      result?: { content?: { text?: string }[]; isError?: boolean };
    };
    try {
      envelope = JSON.parse(text);
    } catch {
      throw new AgentWalletError(`${tool}: Lens answered something other than JSON-RPC`);
    }
    if (envelope.error) {
      throw new AgentWalletError(`${tool}: ${envelope.error.message ?? JSON.stringify(envelope.error)}`);
    }
    const out = envelope.result?.content?.[0]?.text ?? "";
    if (envelope.result?.isError) {
      throw new PaymentRefused(out);
    }
    try {
      return JSON.parse(out) as T;
    } catch {
      throw new AgentWalletError(`${tool}: unreadable result: ${out}`);
    }
  }
}
