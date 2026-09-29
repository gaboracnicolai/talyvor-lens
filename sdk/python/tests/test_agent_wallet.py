"""The agent wallet through the SDK (B19.18).

A local HTTP server answers /mcp the way Lens does (JSON-RPC 2.0; a tool's
answer is JSON in content[0].text; a refusal is a result marked isError),
holding one agent with a 10 LXC balance, approval above 1 LXC and a 5 LXC
limit per request. The SDK talks to it over real HTTP.
"""

from __future__ import annotations

import json
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest

from talyvor_lens import AgentWalletError, LensClient, PaymentRefused

APPROVAL_ABOVE = 1_000_000
MAX_PER_REQUEST = 5_000_000


class FakeLens:
    def __init__(self) -> None:
        self.balance = 10_000_000
        self.approvals: dict[str, dict[str, Any]] = {}
        self.calls: list[dict[str, Any]] = []

    def tool(self, name: str, args: dict[str, Any]) -> Any:
        if name.startswith("wallet_"):  # B22.11: echo what the SDK sent
            return {"tool": name, "arguments": args}
        if name == "agent_balance":
            return {
                "agent": {"id": "agt_buyer", "name": "buyer", "balance_ulxc": self.balance, "spent_ulxc": 0},
                "rules": {"approval_above_ulxc": APPROVAL_ABOVE, "max_per_request_ulxc": MAX_PER_REQUEST},
                "workspace_paused": False,
            }
        fingerprint = (args.get("to_agent_id"), args.get("amount_ulxc"), args.get("memo", ""))
        if name == "agent_request_approval":
            approval = {"id": "apr_1", "agent_id": "agt_buyer", "amount_ulxc": args["amount_ulxc"],
                        "reason": args["reason"], "status": "pending"}
            self.approvals[str(fingerprint)] = approval
            return approval
        if name == "agent_pay":
            amount = args["amount_ulxc"]
            if amount > MAX_PER_REQUEST:
                raise Refusal(f"economy: the agent's spending rules refuse this request: this payment would cost up to "
                              f"{amount / 1e6:g} LXC; the agent's limit per request is 5 LXC")
            approval = self.approvals.get(str(fingerprint))
            if amount > APPROVAL_ABOVE and (approval is None or approval["status"] != "approved"):
                raise Refusal(f"this request would cost up to {amount / 1e6:g} LXC, above the agent's approval amount "
                              f"— approval apr_1 must be approved by the workspace's owner before it is retried")
            if approval is not None:
                approval["status"] = "used"
            self.balance -= amount
            return {"entry_id": "0b6f3c1e-8c1a-4f7e-9d2a-5a1c2b3d4e5f", "from_agent_id": "agt_buyer",
                    "to_agent_id": args["to_agent_id"], "amount_ulxc": amount, "from_balance_ulxc": self.balance}
        return {"entry_id": args["entry_id"], "kind": "pay", "postings": [
            {"posting_id": 1, "account": "agent:agt_buyer", "amount_ulxc": -2_000_000},
            {"posting_id": 2, "account": "agent:agt_seller", "amount_ulxc": 2_000_000},
        ]}


class Refusal(Exception):
    pass


@pytest.fixture
def lens() -> Iterator[tuple[FakeLens, str]]:
    fake = FakeLens()

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self) -> None:
            req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            fake.calls.append({"path": self.path, "auth": self.headers.get("Authorization"), **req})
            if self.headers.get("Authorization") != "Bearer tlv_agent_key":
                self.send_response(401)
                self.end_headers()
                self.wfile.write(b'{"error":"invalid API key"}')
                return
            params = req["params"]
            try:
                result = {"content": [{"type": "text", "text": json.dumps(fake.tool(params["name"], params["arguments"]))}]}
            except Refusal as exc:
                result = {"content": [{"type": "text", "text": str(exc)}], "isError": True}
            body = json.dumps({"jsonrpc": "2.0", "id": req["id"], "result": result}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args: Any) -> None:
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield fake, f"http://127.0.0.1:{server.server_address[1]}"
    server.shutdown()


def test_an_agent_checks_its_balance_asks_for_approval_pays_once_approved_and_is_refused_beyond_its_rules(lens) -> None:
    fake, url = lens
    wallet = LensClient(lens_url=url, api_key="tlv_agent_key").wallet

    balance = wallet.balance()
    assert balance["agent"]["balance_ulxc"] == 10_000_000
    assert balance["rules"]["approval_above_ulxc"] == APPROVAL_ABOVE

    approval = wallet.request_approval("agt_seller", 2_000_000, reason="October hosting", memo="invoice 7")
    assert approval["status"] == "pending" and approval["reason"] == "October hosting"

    with pytest.raises(PaymentRefused, match="apr_1"):
        wallet.pay("agt_seller", 2_000_000, memo="invoice 7")

    fake.approvals[str(("agt_seller", 2_000_000, "invoice 7"))]["status"] = "approved"  # the owner approves
    payment = wallet.pay("agt_seller", 2_000_000, memo="invoice 7")
    assert payment["from_balance_ulxc"] == 8_000_000

    with pytest.raises(PaymentRefused, match="limit per request"):
        wallet.pay("agt_seller", 6_000_000)
    assert wallet.balance()["agent"]["balance_ulxc"] == 8_000_000

    receipt = wallet.receipt(payment["entry_id"])
    assert sum(p["amount_ulxc"] for p in receipt["postings"]) == 0

    # Every call went to /mcp as a tools/call carrying the agent's own key.
    assert {c["path"] for c in fake.calls} == {"/mcp"}
    assert {c["method"] for c in fake.calls} == {"tools/call"}
    assert {c["auth"] for c in fake.calls} == {"Bearer tlv_agent_key"}
    assert [c["params"]["name"] for c in fake.calls] == [
        "agent_balance", "agent_request_approval", "agent_pay", "agent_pay", "agent_pay", "agent_balance", "agent_receipt",
    ]


def test_a_key_lens_rejects_is_an_error_not_a_refusal(lens) -> None:
    _, url = lens
    with pytest.raises(AgentWalletError, match="401") as caught:
        LensClient(lens_url=url, api_key="tlv_wrong").wallet.balance()
    assert not isinstance(caught.value, PaymentRefused)


def test_every_wallet_capability_calls_its_tool_with_the_agents_own_key(lens) -> None:
    fake, url = lens
    wallet = LensClient(lens_url=url, api_key="tlv_agent_key").wallet
    cases = [
        (lambda: wallet.send("@seller", 1_000_000, memo="hosting"), "wallet_send",
         {"to": "@seller", "amount_ulxc": 1_000_000, "memo": "hosting"}),
        (lambda: wallet.request("@buyer", 2_000_000), "wallet_request", {"from": "@buyer", "amount_ulxc": 2_000_000, "memo": ""}),
        (lambda: wallet.requests(), "wallet_requests", {}),
        (lambda: wallet.answer_request("mreq_1", accept=False), "wallet_answer_request", {"request_id": "mreq_1", "accept": False}),
        (lambda: wallet.refund("xfer_1"), "wallet_refund", {"transfer_id": "xfer_1"}),
        (lambda: wallet.credit_line(), "wallet_credit_line", {}),
        (lambda: wallet.offer_loan("@co", 30_000_000, 3, "week", interest_bps=500), "wallet_offer_loan",
         {"to": "@co", "principal_ulxc": 30_000_000, "instalments": 3, "every": "week", "interest_bps": 500,
          "late_fee_ulxc": 0, "memo": ""}),
        (lambda: wallet.loans(), "wallet_loans", {}),
        (lambda: wallet.answer_loan("loan_1", accept=True), "wallet_answer_loan", {"loan_id": "loan_1", "accept": True}),
        (lambda: wallet.escrow_pay("@seller", 5_000_000, "2026-10-06T12:00:00Z", memo="logo"), "wallet_escrow_pay",
         {"to": "@seller", "amount_ulxc": 5_000_000, "release_at": "2026-10-06T12:00:00Z", "memo": "logo"}),
        (lambda: wallet.escrows(), "wallet_escrows", {}),
        (lambda: wallet.escrow_confirm("escrow_1"), "wallet_escrow_confirm", {"escrow_id": "escrow_1"}),
        (lambda: wallet.escrow_dispute("escrow_1", "never delivered"), "wallet_escrow_dispute",
         {"escrow_id": "escrow_1", "reason": "never delivered"}),
        (lambda: wallet.pots(), "wallet_pots", {}),
        (lambda: wallet.pot_create("laptop", "goal", target_ulxc=50_000_000), "wallet_pot_create",
         {"name": "laptop", "kind": "goal", "target_ulxc": 50_000_000}),
        (lambda: wallet.pot_move("pot_1", 1_000_000, "in"), "wallet_pot_move", {"pot_id": "pot_1", "amount_ulxc": 1_000_000, "direction": "in"}),
        (lambda: wallet.quotes(), "wallet_quotes", {}),
        (lambda: wallet.portfolio_open("fx", 1_000_000_000), "wallet_portfolio_open", {"name": "fx", "cash_uusd": 1_000_000_000}),
        (lambda: wallet.portfolios(), "wallet_portfolios", {}),
        (lambda: wallet.order("pf_1", "EUR", "buy", "limit", 100_000_000, limit_price_usd="1.15"), "wallet_order",
         {"portfolio_id": "pf_1", "instrument": "EUR", "side": "buy", "type": "limit", "quantity_micros": 100_000_000,
          "limit_price_usd": "1.15"}),
        (lambda: wallet.order_cancel("pf_1", "ord_1"), "wallet_order_cancel", {"portfolio_id": "pf_1", "order_id": "ord_1"}),
    ]
    for method, tool, arguments in cases:
        assert method() == {"tool": tool, "arguments": arguments}
    assert {c["auth"] for c in fake.calls} == {"Bearer tlv_agent_key"}
