"""Type definitions and header constants for the Talyvor Lens SDK.

The HEADER_* constants are the single source of truth for the wire-level
header names. Keep them in sync with the Go server-side handlers in
internal/proxy and internal/workspace — if the server adds a new
X-Talyvor-* header, mirror it here so SDK users can set it via kwargs.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional

HEADER_AUTHORIZATION = "Authorization"
HEADER_WORKSPACE = "X-Talyvor-Workspace"
HEADER_TEAM = "X-Talyvor-Team"
HEADER_FEATURE = "X-Talyvor-Feature"
HEADER_SESSION = "X-Talyvor-Session"
HEADER_AGENT = "X-Talyvor-Agent"
HEADER_BRANCH = "X-Talyvor-Branch"
HEADER_PR = "X-Talyvor-PR"
HEADER_COMMIT = "X-Talyvor-Commit"
HEADER_REPOSITORY = "X-Talyvor-Repository"


@dataclass(frozen=True)
class AttributionContext:
    """Aggregates the request-level attribution fields a caller can set.

    Frozen so two LensClients constructed from the same context can't
    accidentally share mutable state.
    """

    workspace_id: str = "default"
    team: str = ""
    feature: str = ""
    session_id: str = ""
    agent_name: str = ""
    branch: str = ""
    pr_number: str = ""
    commit: str = ""
    repository: str = ""

    def is_empty(self) -> bool:
        """True when no attribution fields beyond the default workspace are set."""
        return (
            self.workspace_id in ("", "default")
            and not any(
                (
                    self.team,
                    self.feature,
                    self.session_id,
                    self.agent_name,
                    self.branch,
                    self.pr_number,
                    self.commit,
                    self.repository,
                )
            )
        )
