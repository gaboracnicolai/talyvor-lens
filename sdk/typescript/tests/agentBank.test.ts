/**
 * The agent bank through the SDK (B19.18).
 *
 * A local HTTP server answers /mcp the way Lens does (JSON-RPC 2.0; a tool's
 * answer is JSON in content[0].text; a refusal is a result marked isError),
 * holding one agent with a 10 LXC balance, approval above 1 LXC and a 5 LXC
 * limit per request. The SDK talks to it over real HTTP.
 */

import http from "http";
import type { AddressInfo } from "net";
import { AgentBankError, LensClient, PaymentRefused } from "../src";

const APPROVAL_ABOVE = 1_000_000;
const MAX_PER_REQUEST = 5_000_000;

class Refusal extends Error {}

type Args = Record<string, any>;

class FakeLens {
  balance = 10_000_000;
  approvals = new Map<string, Args>();
  calls: { path?: string; auth?: string; method: string; params: Args }[] = [];

  tool(name: string, args: Args): unknown {
    if (name === "agent_balance") {
      return {
        agent: { id: "agt_buyer", name: "buyer", balance_ulxc: this.balance, spent_ulxc: 0 },
        rules: { approval_above_ulxc: APPROVAL_ABOVE, max_per_request_ulxc: MAX_PER_REQUEST },
        workspace_paused: false,
      };
    }
    const fingerprint = `${args.to_agent_id}|${args.amount_ulxc}|${args.memo ?? ""}`;
    if (name === "agent_request_approval") {
      const approval = { id: "apr_1", agent_id: "agt_buyer", amount_ulxc: args.amount_ulxc, reason: args.reason, status: "pending" };
      this.approvals.set(fingerprint, approval);
      return approval;
    }
    if (name === "agent_pay") {
      const amount: number = args.amount_ulxc;
      if (amount > MAX_PER_REQUEST) {
        throw new Refusal(`economy: the agent's spending rules refuse this request: this payment would cost up to ${amount / 1e6} LXC; the agent's limit per request is 5 LXC`);
      }
      const approval = this.approvals.get(fingerprint);
      if (amount > APPROVAL_ABOVE && approval?.status !== "approved") {
        throw new Refusal(`this request would cost up to ${amount / 1e6} LXC, above the agent's approval amount — approval apr_1 must be approved by the workspace's owner before it is retried`);
      }
      if (approval) approval.status = "used";
      this.balance -= amount;
      return { entry_id: "0b6f3c1e-8c1a-4f7e-9d2a-5a1c2b3d4e5f", from_agent_id: "agt_buyer", to_agent_id: args.to_agent_id, amount_ulxc: amount, from_balance_ulxc: this.balance };
    }
    return {
      entry_id: args.entry_id,
      kind: "pay",
      postings: [
        { posting_id: 1, account: "agent:agt_buyer", amount_ulxc: -2_000_000 },
        { posting_id: 2, account: "agent:agt_seller", amount_ulxc: 2_000_000 },
      ],
    };
  }
}

describe("AgentBank", () => {
  let fake: FakeLens;
  let server: http.Server;
  let url: string;

  beforeEach(async () => {
    fake = new FakeLens();
    server = http.createServer((req, res) => {
      let raw = "";
      req.on("data", (chunk) => (raw += chunk));
      req.on("end", () => {
        const rpc = JSON.parse(raw);
        fake.calls.push({ path: req.url, auth: req.headers.authorization, method: rpc.method, params: rpc.params });
        if (req.headers.authorization !== "Bearer tlv_agent_key") {
          res.writeHead(401).end('{"error":"invalid API key"}');
          return;
        }
        let result;
        try {
          result = { content: [{ type: "text", text: JSON.stringify(fake.tool(rpc.params.name, rpc.params.arguments)) }] };
        } catch (err) {
          result = { content: [{ type: "text", text: (err as Error).message }], isError: true };
        }
        res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify({ jsonrpc: "2.0", id: rpc.id, result }));
      });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });

  afterEach(() => new Promise<void>((resolve) => server.close(() => resolve())));

  it("an agent checks its balance, asks for approval, pays once approved and is refused beyond its rules", async () => {
    const bank = new LensClient({ lensUrl: url, apiKey: "tlv_agent_key" }).bank;

    const balance = await bank.balance();
    expect(balance.agent.balance_ulxc).toBe(10_000_000);
    expect(balance.rules.approval_above_ulxc).toBe(APPROVAL_ABOVE);

    const approval = await bank.requestApproval("agt_seller", 2_000_000, "October hosting", "invoice 7");
    expect(approval.status).toBe("pending");
    expect(approval.reason).toBe("October hosting");

    await expect(bank.pay("agt_seller", 2_000_000, "invoice 7")).rejects.toThrow(PaymentRefused);

    fake.approvals.get("agt_seller|2000000|invoice 7")!.status = "approved"; // the owner approves
    const payment = await bank.pay("agt_seller", 2_000_000, "invoice 7");
    expect(payment.from_balance_ulxc).toBe(8_000_000);

    await expect(bank.pay("agt_seller", 6_000_000)).rejects.toThrow(/limit per request/);
    expect((await bank.balance()).agent.balance_ulxc).toBe(8_000_000);

    const receipt = await bank.receipt(payment.entry_id);
    expect(receipt.postings.reduce((sum, p) => sum + p.amount_ulxc, 0)).toBe(0);

    // Every call went to /mcp as a tools/call carrying the agent's own key.
    expect(new Set(fake.calls.map((c) => c.path))).toEqual(new Set(["/mcp"]));
    expect(new Set(fake.calls.map((c) => c.method))).toEqual(new Set(["tools/call"]));
    expect(new Set(fake.calls.map((c) => c.auth))).toEqual(new Set(["Bearer tlv_agent_key"]));
    expect(fake.calls.map((c) => c.params.name)).toEqual([
      "agent_balance", "agent_request_approval", "agent_pay", "agent_pay", "agent_pay", "agent_balance", "agent_receipt",
    ]);
  });

  it("a key Lens rejects is an error, not a refusal", async () => {
    const err = await new LensClient({ lensUrl: url, apiKey: "tlv_wrong" }).bank.balance().catch((e) => e);
    expect(err).toBeInstanceOf(AgentBankError);
    expect(err).not.toBeInstanceOf(PaymentRefused);
    expect(err.message).toMatch(/401/);
  });
});
