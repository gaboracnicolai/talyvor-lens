"""AgentWallet — an agent's own wallet at Talyvor Lens.

An agent calls these with ITS OWN key (a key attached to the agent): its
balance and spending rules, an approval asked for with a reason, a payment
to another agent of its workspace, and the receipt of an entry it is party
to. The agent is the key's, never an argument, so a key reaches only its
own account; every call — served or refused — is logged by Lens.

They are Lens's MCP tools (agent_balance, agent_request_approval,
agent_pay, agent_receipt, and B22.11's wallet_* tools) called over JSON-RPC at
``/mcp``, so an agent using the SDK and an agent using MCP are judged by the
same rules. The wallet_* methods reach every capability of the wallet: send
and request credits, give a transfer back, the company's credit line, loans,
escrow, pots, and simulated portfolios and orders. Each answers with Lens's
JSON; a refusal — the agent's rules, or a capability's class — raises
:class:`PaymentRefused` saying why.
"""

from __future__ import annotations

import itertools
import json
from datetime import datetime
from typing import Any

import httpx

from .types import AgentApproval, AgentBalance, AgentPayment, AgentReceipt


class AgentWalletError(Exception):
    """Lens could not answer the call (network, credential, protocol)."""


class PaymentRefused(AgentWalletError):
    """Lens refused what the agent asked, and says why.

    For example: the payment needs an approval that is still pending, it
    is beyond the agent's limit per request, or the agent is paused.
    """


class AgentWallet:
    """The agent wallet tools, called with the client's key.

    Reach it as ``LensClient(...).wallet``; the key must be attached to an
    agent. Amounts are in µLXC (1 LXC = 1,000,000 µLXC).

    Example:
        >>> wallet = LensClient(lens_url="https://lens.talyvor.com", api_key="tlv_agent_key").wallet
        >>> wallet.balance()["agent"]["balance_ulxc"]
        10000000
        >>> approval = wallet.request_approval("agt_seller", 2_000_000, reason="October hosting")
        >>> # ...once a person approves it on the Agent Wallets screen:
        >>> payment = wallet.pay("agt_seller", 2_000_000)
        >>> wallet.receipt(payment["entry_id"])["postings"]
    """

    def __init__(self, lens_url: str, headers: dict[str, str], timeout: float = 30.0) -> None:
        self._url = f"{lens_url}/mcp"
        self._headers = dict(headers)
        self._timeout = timeout
        self._ids = itertools.count(1)

    def balance(self) -> AgentBalance:
        """The agent's account: balance, spent, paused, and its spending rules."""
        return self._call("agent_balance", {})

    def request_approval(self, to_agent_id: str, amount_ulxc: int, reason: str, memo: str = "") -> AgentApproval:
        """Ask a person to approve a payment above the agent's approval amount.

        Once approved, make exactly that payment — same recipient, amount
        and memo — with :meth:`pay`. Asking again for the same payment
        returns the approval already open, with the new reason.
        """
        return self._call(
            "agent_request_approval",
            {"to_agent_id": to_agent_id, "amount_ulxc": amount_ulxc, "memo": memo, "reason": reason},
        )

    def pay(self, to_agent_id: str, amount_ulxc: int, memo: str = "") -> AgentPayment:
        """Pay another agent of the workspace from the agent's own balance.

        Raises :class:`PaymentRefused` when the agent's rules refuse it.
        """
        return self._call("agent_pay", {"to_agent_id": to_agent_id, "amount_ulxc": amount_ulxc, "memo": memo})

    def receipt(self, entry_id: str) -> AgentReceipt:
        """Every posting of an entry on the agent's account; they sum to zero."""
        return self._call("agent_receipt", {"entry_id": entry_id})

    # B22.11 — every capability of the wallet. Addresses are a wallet id or an @handle.

    def send(self, to: str, amount_ulxc: int, memo: str = "") -> dict[str, Any]:
        """Send credits to any agent's wallet on Talyvor, within the agent's spending rules."""
        return self._call("wallet_send", {"to": to, "amount_ulxc": amount_ulxc, "memo": memo})

    def request(self, from_wallet: str, amount_ulxc: int, memo: str = "") -> dict[str, Any]:
        """Ask another agent's wallet for credits; it accepts or declines."""
        return self._call("wallet_request", {"from": from_wallet, "amount_ulxc": amount_ulxc, "memo": memo})

    def requests(self) -> dict[str, Any]:
        """The requests for credits the agent made and was made."""
        return self._call("wallet_requests", {})

    def answer_request(self, request_id: str, accept: bool) -> dict[str, Any]:
        """Accept (pay, within the agent's rules) or decline a request made of the agent."""
        return self._call("wallet_answer_request", {"request_id": request_id, "accept": accept})

    def refund(self, transfer_id: str) -> dict[str, Any]:
        """Give a transfer the agent received back to its sender, once."""
        return self._call("wallet_refund", {"transfer_id": transfer_id})

    def credit_line(self) -> dict[str, Any]:
        """The company's credit line from Talyvor: limit, used and available."""
        return self._call("wallet_credit_line", {})

    def offer_loan(
        self,
        to: str,
        principal_ulxc: int,
        instalments: int,
        every: str,
        interest_bps: int = 0,
        late_fee_ulxc: int = 0,
        memo: str = "",
    ) -> dict[str, Any]:
        """Offer another company's agent a loan, repaid in equal instalments every day, week or month."""
        return self._call("wallet_offer_loan", {
            "to": to, "principal_ulxc": principal_ulxc, "instalments": instalments, "every": every,
            "interest_bps": interest_bps, "late_fee_ulxc": late_fee_ulxc, "memo": memo,
        })

    def loans(self) -> dict[str, Any]:
        """The loans the agent lent and borrowed, with every instalment."""
        return self._call("wallet_loans", {})

    def answer_loan(self, loan_id: str, accept: bool) -> dict[str, Any]:
        """Accept (the principal is paid to the agent) or decline a loan offered to it."""
        return self._call("wallet_answer_loan", {"loan_id": loan_id, "accept": accept})

    def escrow_pay(self, to: str, amount_ulxc: int, release_at: datetime | str, memo: str = "") -> dict[str, Any]:
        """Pay into escrow for another agent: held until confirmed, or released at release_at undisputed."""
        at = release_at.isoformat() if isinstance(release_at, datetime) else release_at
        return self._call("wallet_escrow_pay", {"to": to, "amount_ulxc": amount_ulxc, "release_at": at, "memo": memo})

    def escrows(self) -> dict[str, Any]:
        """The escrows the agent paid into and is owed, with each state."""
        return self._call("wallet_escrows", {})

    def escrow_confirm(self, escrow_id: str) -> dict[str, Any]:
        """Confirm delivery on an escrow the agent paid into: it is released to the payee now."""
        return self._call("wallet_escrow_confirm", {"escrow_id": escrow_id})

    def escrow_dispute(self, escrow_id: str, reason: str) -> dict[str, Any]:
        """Dispute an escrow the agent paid into, before its deadline: held until Talyvor's operator decides."""
        return self._call("wallet_escrow_dispute", {"escrow_id": escrow_id, "reason": reason})

    def pots(self) -> dict[str, Any]:
        """The agent's pots and what each holds."""
        return self._call("wallet_pots", {})

    def pot_create(
        self, name: str, kind: str, target_ulxc: int = 0, locked_until: datetime | str | None = None
    ) -> dict[str, Any]:
        """Make a pot — a goal, a budget or a reserve — optionally locked until a date."""
        args: dict[str, Any] = {"name": name, "kind": kind, "target_ulxc": target_ulxc}
        if locked_until is not None:
            args["locked_until"] = locked_until.isoformat() if isinstance(locked_until, datetime) else locked_until
        return self._call("wallet_pot_create", args)

    def pot_move(self, pot_id: str, amount_ulxc: int, direction: str) -> dict[str, Any]:
        """Move credits into a pot ("in") or back out of it ("out", unless it is locked)."""
        return self._call("wallet_pot_move", {"pot_id": pot_id, "amount_ulxc": amount_ulxc, "direction": direction})

    def quotes(self) -> dict[str, Any]:
        """The simulated market's instruments and their prices in USD, from the ECB's reference rates."""
        return self._call("wallet_quotes", {})

    def portfolio_open(self, name: str, cash_uusd: int) -> dict[str, Any]:
        """Open a simulated portfolio with simulated US dollars (µUSD). No money or credits move."""
        return self._call("wallet_portfolio_open", {"name": name, "cash_uusd": cash_uusd})

    def portfolios(self) -> dict[str, Any]:
        """The agent's simulated portfolios, valued at the latest quotes, with every order."""
        return self._call("wallet_portfolios", {})

    def order(
        self,
        portfolio_id: str,
        instrument: str,
        side: str,
        type: str,
        quantity_micros: int,
        limit_price_usd: str | None = None,
    ) -> dict[str, Any]:
        """Place a market or limit order in a simulated portfolio. Nothing is sent to a market."""
        args: dict[str, Any] = {"portfolio_id": portfolio_id, "instrument": instrument, "side": side, "type": type,
                                "quantity_micros": quantity_micros}
        if limit_price_usd is not None:
            args["limit_price_usd"] = limit_price_usd
        return self._call("wallet_order", args)

    def order_cancel(self, portfolio_id: str, order_id: str) -> dict[str, Any]:
        """Cancel an open order in a simulated portfolio."""
        return self._call("wallet_order_cancel", {"portfolio_id": portfolio_id, "order_id": order_id})

    def _call(self, tool: str, arguments: dict[str, Any]) -> Any:
        body = {
            "jsonrpc": "2.0",
            "id": next(self._ids),
            "method": "tools/call",
            "params": {"name": tool, "arguments": arguments},
        }
        try:
            resp = httpx.post(self._url, json=body, headers=self._headers, timeout=self._timeout)
        except httpx.HTTPError as exc:
            raise AgentWalletError(f"{tool}: {exc}") from exc
        if resp.status_code != 200:
            raise AgentWalletError(f"{tool}: Lens answered {resp.status_code}: {resp.text.strip()}")
        try:
            envelope = resp.json()
        except ValueError as exc:
            raise AgentWalletError(f"{tool}: Lens answered something other than JSON-RPC") from exc
        if envelope.get("error"):
            raise AgentWalletError(f"{tool}: {envelope['error'].get('message', envelope['error'])}")
        result = envelope.get("result") or {}
        content = result.get("content") or [{}]
        text = content[0].get("text", "")
        if result.get("isError"):
            raise PaymentRefused(text)
        try:
            return json.loads(text)
        except ValueError as exc:
            raise AgentWalletError(f"{tool}: unreadable result: {text!r}") from exc
