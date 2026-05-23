"""LensClient — drop-in replacement for OpenAI / Anthropic clients.

The client is a thin facade: it constructs headers once on init and
hands them to lazily-instantiated OpenAI / Anthropic clients. Importing
the OpenAI or Anthropic library is deferred to property access so a
LensClient built for OpenAI-only doesn't crash when the anthropic
package isn't installed.
"""

from __future__ import annotations

from typing import Any, Dict

from .middleware import inject_lens_headers
from .types import HEADER_BRANCH, HEADER_PR


class LensClient:
    """Drop-in replacement for OpenAI / Anthropic clients.

    Routes all requests through Talyvor Lens. The familiar
    ``client.openai.chat.completions.create(...)`` shape works
    unchanged — Lens sits transparently in front, adding caching,
    routing, attribution, and cost tracking.

    Example:
        >>> client = LensClient(
        ...     lens_url="http://lens:8080",
        ...     api_key="tlv_...",
        ...     workspace_id="my-team",
        ... )
        >>> response = client.openai.chat.completions.create(
        ...     model="gpt-4o",
        ...     messages=[{"role": "user", "content": "Hello"}],
        ... )
    """

    def __init__(
        self,
        lens_url: str,
        api_key: str,
        workspace_id: str = "default",
        team: str = "",
        feature: str = "",
        session_id: str = "",
        agent_name: str = "",
        branch: str = "",
    ) -> None:
        if not lens_url:
            raise ValueError("lens_url is required")
        if not api_key:
            raise ValueError("api_key is required")
        self.lens_url = lens_url.rstrip("/")
        self.api_key = api_key
        self.workspace_id = workspace_id or "default"
        self._headers: Dict[str, str] = inject_lens_headers(
            api_key=api_key,
            workspace_id=self.workspace_id,
            team=team,
            feature=feature,
            session_id=session_id,
            agent_name=agent_name,
            branch=branch,
        )
        # Lazy clients — instantiated on first property access so an
        # OpenAI-only deployment doesn't need the anthropic package.
        self._openai_client: Any = None
        self._anthropic_client: Any = None

    @property
    def openai(self) -> Any:
        """OpenAI client configured to talk to Lens."""
        if self._openai_client is None:
            from openai import OpenAI  # local import — see class docstring

            self._openai_client = OpenAI(
                base_url=f"{self.lens_url}/v1/proxy/openai",
                api_key=self.api_key,
                default_headers=self._headers,
            )
        return self._openai_client

    @property
    def anthropic(self) -> Any:
        """Anthropic client configured to talk to Lens.

        Requires the optional ``anthropic`` extra to be installed:
        ``pip install talyvor-lens[anthropic]``.
        """
        if self._anthropic_client is None:
            from anthropic import Anthropic  # local import — optional dep

            self._anthropic_client = Anthropic(
                base_url=f"{self.lens_url}/v1/proxy/anthropic",
                api_key=self.api_key,
                default_headers=self._headers,
            )
        return self._anthropic_client

    def get_headers(self) -> Dict[str, str]:
        """Return a copy of the headers Lens will add to every request.

        Useful when you want to use a different HTTP client (httpx,
        requests, aiohttp) while still getting the right Lens routing.
        """
        return dict(self._headers)

    def set_session(self, session_id: str, agent_name: str = "") -> "LensClient":
        """Return a new client with session attribution overridden.

        The original client's headers are preserved — only the session
        and agent fields change. Useful inside an agent framework where
        each turn carries its own session ID.
        """
        return self._derive(session_id=session_id, agent_name=agent_name)

    def set_branch(self, branch: str, pr_number: str = "") -> "LensClient":
        """Return a new client with Git attribution headers set.

        Branch and (optional) PR number flow through to the proxy's
        attribution tracker so cost shows up on the right PR's CI run.
        """
        clone = self._derive(branch=branch)
        if pr_number:
            clone._headers[HEADER_PR] = pr_number
        return clone

    def _derive(self, **overrides: str) -> "LensClient":
        """Construct a new LensClient inheriting non-overridden state.

        Internal so callers go through the named ``set_*`` methods,
        which carry semantic meaning the kwargs don't.
        """
        # Start with the current header set; rebuild via the constructor
        # so the new client picks up any future header additions made in
        # __init__ without us having to mirror them here.
        defaults = {
            "lens_url": self.lens_url,
            "api_key": self.api_key,
            "workspace_id": self.workspace_id,
            "team": self._headers.get("X-Talyvor-Team", ""),
            "feature": self._headers.get("X-Talyvor-Feature", ""),
            "session_id": self._headers.get("X-Talyvor-Session", ""),
            "agent_name": self._headers.get("X-Talyvor-Agent", ""),
            "branch": self._headers.get(HEADER_BRANCH, ""),
        }
        defaults.update(overrides)
        return LensClient(**defaults)
