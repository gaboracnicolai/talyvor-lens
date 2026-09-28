"""AgentBank — an agent's own account at Talyvor Lens.

An agent calls these with ITS OWN key (a key attached to the agent): its
balance and spending rules, an approval asked for with a reason, a payment
to another agent of its workspace, and the receipt of an entry it is party
to. The agent is the key's, never an argument, so a key reaches only its
own account; every call — served or refused — is logged by Lens.

They are Lens's MCP tools (agent_balance, agent_request_approval,
agent_pay, agent_receipt) called over JSON-RPC at ``/mcp``, so an agent
using the SDK and an agent using MCP are judged by the same rules.
"""

from __future__ import annotations

import itertools
import json
from typing import Any

import httpx

from .types import AgentApproval, AgentBalance, AgentPayment, AgentReceipt


class AgentBankError(Exception):
    """Lens could not answer the call (network, credential, protocol)."""


class PaymentRefused(AgentBankError):
    """The bank refused what the agent asked, and says why.

    For example: the payment needs an approval that is still pending, it
    is beyond the agent's limit per request, or the agent is paused.
    """


class AgentBank:
    """The agent bank tools, called with the client's key.

    Reach it as ``LensClient(...).bank``; the key must be attached to an
    agent. Amounts are in µLXC (1 LXC = 1,000,000 µLXC).

    Example:
        >>> bank = LensClient(lens_url="https://lens.talyvor.com", api_key="tlv_agent_key").bank
        >>> bank.balance()["agent"]["balance_ulxc"]
        10000000
        >>> approval = bank.request_approval("agt_seller", 2_000_000, reason="October hosting")
        >>> # ...once a person approves it on the Agent Bank screen:
        >>> payment = bank.pay("agt_seller", 2_000_000)
        >>> bank.receipt(payment["entry_id"])["postings"]
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
            raise AgentBankError(f"{tool}: {exc}") from exc
        if resp.status_code != 200:
            raise AgentBankError(f"{tool}: Lens answered {resp.status_code}: {resp.text.strip()}")
        try:
            envelope = resp.json()
        except ValueError as exc:
            raise AgentBankError(f"{tool}: Lens answered something other than JSON-RPC") from exc
        if envelope.get("error"):
            raise AgentBankError(f"{tool}: {envelope['error'].get('message', envelope['error'])}")
        result = envelope.get("result") or {}
        content = result.get("content") or [{}]
        text = content[0].get("text", "")
        if result.get("isError"):
            raise PaymentRefused(text)
        try:
            return json.loads(text)
        except ValueError as exc:
            raise AgentBankError(f"{tool}: unreadable result: {text!r}") from exc
