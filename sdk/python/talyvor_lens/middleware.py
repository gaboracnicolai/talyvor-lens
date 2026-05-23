"""Standalone header-injection helper.

Use ``inject_lens_headers`` when you already have an HTTP client (httpx,
requests, aiohttp) and want to add the Talyvor Lens routing headers
without instantiating the full ``LensClient``. Empty/None values are
omitted so the request doesn't carry blank tags.
"""

from __future__ import annotations

from typing import Any, Dict

from .types import (
    HEADER_AGENT,
    HEADER_AUTHORIZATION,
    HEADER_BRANCH,
    HEADER_COMMIT,
    HEADER_FEATURE,
    HEADER_PR,
    HEADER_REPOSITORY,
    HEADER_SESSION,
    HEADER_TEAM,
    HEADER_WORKSPACE,
)

_OPTIONAL_HEADER_MAP: tuple[tuple[str, str], ...] = (
    ("team", HEADER_TEAM),
    ("feature", HEADER_FEATURE),
    ("session_id", HEADER_SESSION),
    ("agent_name", HEADER_AGENT),
    ("branch", HEADER_BRANCH),
    ("pr_number", HEADER_PR),
    ("commit", HEADER_COMMIT),
    ("repository", HEADER_REPOSITORY),
)


def inject_lens_headers(
    headers: Dict[str, str] | None = None,
    *,
    api_key: str,
    workspace_id: str = "default",
    **kwargs: Any,
) -> Dict[str, str]:
    """Return a new dict with Lens routing headers merged in.

    The input ``headers`` mapping is copied — the caller's dict is never
    mutated, so passing the same default dict to multiple requests is
    safe.

    Args:
        headers: Pre-existing headers to merge into. Defaults to empty.
        api_key: Talyvor Lens API key (sets the Authorization header).
        workspace_id: Workspace ID for cache + policy scoping.
        **kwargs: Optional attribution fields. Recognised keys:
            ``team``, ``feature``, ``session_id``, ``agent_name``,
            ``branch``, ``pr_number``, ``commit``, ``repository``.
            Empty/None values are silently dropped.

    Returns:
        A new dict combining the input headers with the Lens additions.
    """
    result: Dict[str, str] = dict(headers or {})
    result[HEADER_AUTHORIZATION] = f"Bearer {api_key}"
    result[HEADER_WORKSPACE] = workspace_id or "default"
    for kwarg_name, header_name in _OPTIONAL_HEADER_MAP:
        value = kwargs.get(kwarg_name)
        if value:
            result[header_name] = str(value)
    return result
