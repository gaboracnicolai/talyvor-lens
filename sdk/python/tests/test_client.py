"""Tests for LensClient.

The tests stay at the header-construction layer so they don't depend on
having the openai or anthropic package importable. The lazy property
indirection means we can test everything Lens-specific without ever
instantiating a real OpenAI client.
"""

from __future__ import annotations

import pytest

from talyvor_lens import LensClient


def test_lens_client_requires_url_and_key() -> None:
    with pytest.raises(ValueError):
        LensClient(lens_url="", api_key="tlv_x")
    with pytest.raises(ValueError):
        LensClient(lens_url="http://lens:8080", api_key="")


def test_lens_client_strips_trailing_slash_from_url() -> None:
    client = LensClient(lens_url="http://lens:8080/", api_key="tlv_x")
    assert client.lens_url == "http://lens:8080"


def test_lens_client_sets_authorization_header() -> None:
    client = LensClient(lens_url="http://lens:8080", api_key="tlv_secret")
    headers = client.get_headers()
    assert headers["Authorization"] == "Bearer tlv_secret"


def test_lens_client_sets_workspace_header() -> None:
    client = LensClient(
        lens_url="http://lens:8080",
        api_key="tlv_x",
        workspace_id="finance",
    )
    headers = client.get_headers()
    assert headers["X-Talyvor-Workspace"] == "finance"


def test_lens_client_includes_optional_attribution_headers() -> None:
    client = LensClient(
        lens_url="http://lens:8080",
        api_key="tlv_x",
        workspace_id="ws",
        team="core",
        feature="search",
        session_id="sess-42",
        agent_name="ranger",
        branch="main",
    )
    headers = client.get_headers()
    assert headers["X-Talyvor-Team"] == "core"
    assert headers["X-Talyvor-Feature"] == "search"
    assert headers["X-Talyvor-Session"] == "sess-42"
    assert headers["X-Talyvor-Agent"] == "ranger"
    assert headers["X-Talyvor-Branch"] == "main"


def test_lens_client_omits_unset_optional_headers() -> None:
    client = LensClient(lens_url="http://lens:8080", api_key="tlv_x")
    headers = client.get_headers()
    for forbidden in (
        "X-Talyvor-Team",
        "X-Talyvor-Feature",
        "X-Talyvor-Session",
        "X-Talyvor-Agent",
        "X-Talyvor-Branch",
    ):
        assert forbidden not in headers


def test_set_session_returns_new_client_with_session_header() -> None:
    parent = LensClient(
        lens_url="http://lens:8080",
        api_key="tlv_x",
        workspace_id="ws",
        team="core",
    )
    child = parent.set_session("sess-99", agent_name="planner")

    assert child is not parent
    assert child.get_headers()["X-Talyvor-Session"] == "sess-99"
    assert child.get_headers()["X-Talyvor-Agent"] == "planner"
    # Inherited workspace + team carry across.
    assert child.get_headers()["X-Talyvor-Workspace"] == "ws"
    assert child.get_headers()["X-Talyvor-Team"] == "core"
    # Parent unchanged.
    assert "X-Talyvor-Session" not in parent.get_headers()


def test_set_branch_returns_new_client_with_branch_and_pr() -> None:
    parent = LensClient(lens_url="http://lens:8080", api_key="tlv_x")
    child = parent.set_branch("feat/login", pr_number="42")

    assert child.get_headers()["X-Talyvor-Branch"] == "feat/login"
    assert child.get_headers()["X-Talyvor-PR"] == "42"
    assert "X-Talyvor-Branch" not in parent.get_headers()


def test_get_headers_returns_a_copy() -> None:
    client = LensClient(lens_url="http://lens:8080", api_key="tlv_x")
    headers = client.get_headers()
    headers["X-Tamper"] = "yes"
    assert "X-Tamper" not in client.get_headers()


def test_openai_property_lazy_no_import_if_unused() -> None:
    # Just constructing the client must NOT import openai. We can't
    # reliably uninstall openai inside the test, but we can assert the
    # underlying client field is None until property access.
    client = LensClient(lens_url="http://lens:8080", api_key="tlv_x")
    assert client._openai_client is None
    assert client._anthropic_client is None
