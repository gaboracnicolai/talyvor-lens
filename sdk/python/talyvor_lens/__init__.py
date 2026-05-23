"""Talyvor Lens SDK.

Drop-in OpenAI/Anthropic client wrappers that route every request
through Talyvor Lens, automatically setting the workspace, session,
team, feature, and Git attribution headers the proxy needs.
"""

from .client import LensClient
from .middleware import inject_lens_headers
from .types import AttributionContext

__version__ = "0.1.0"
__all__ = ["LensClient", "inject_lens_headers", "AttributionContext"]
