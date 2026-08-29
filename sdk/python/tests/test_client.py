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


def test_set_branch_then_set_session_keeps_the_pr_number() -> None:
    """PR attribution must survive a later ``set_session``.

    ⚠ THIS IS THE DOCUMENTED USAGE, NOT AN EDGE CASE. ``set_branch``'s own docstring says the PR
    number flows through "so cost shows up on the right PR's CI run", and ``set_session``'s says it
    is "useful inside an agent framework where each turn carries its own session ID" — i.e. set the
    branch once per CI run, derive a session per turn. In that order every turn after the first
    lost X-Talyvor-PR, silently, because ``_derive`` rebuilds through ``__init__`` and the
    constructor has no ``pr_number`` parameter. The reverse order kept it, so the defect depended
    on which of two documented calls came second.

    The consequence is not a crash: the request succeeds, the proxy stores an attribution row with
    an empty pr_number, and `summaryByPRSQL` shows the CI run's spend against no PR at all.
    """
    ci = LensClient(lens_url="http://lens:8080", api_key="tlv_x").set_branch("feat/x", "42")
    assert ci.get_headers()["X-Talyvor-PR"] == "42"

    per_turn = ci.set_session("turn-1", agent_name="planner")
    assert per_turn.get_headers().get("X-Talyvor-PR") == "42", (
        "set_session dropped the PR number set by set_branch"
    )
    # and the session override still took effect, so the fix is not "return the same client"
    assert per_turn.get_headers()["X-Talyvor-Session"] == "turn-1"
    assert per_turn.get_headers()["X-Talyvor-Agent"] == "planner"
    # branch survives too — it always did, and a fix that broke it would be a regression
    assert per_turn.get_headers()["X-Talyvor-Branch"] == "feat/x"


def test_derive_override_beats_the_inherited_header() -> None:
    """A carried-over header must never beat the value the caller just set.

    ⚠ THIS CASE EXISTS BECAUSE A CONTROL FOUND ITS ABSENCE. The chaining test above starts from a
    client with NO session, so the parent has no X-Talyvor-Session key at all — which means a fix
    that copied the parent's headers OVER the rebuilt ones instead of into the gaps would have
    passed it. The mutation harness injected exactly that fix and the suite stayed green. The
    parent here HAS a session and an agent, so the override is observable and the wrong fix reds.
    """
    base = LensClient(
        lens_url="http://lens:8080",
        api_key="tlv_x",
        session_id="turn-0",
        agent_name="scout",
    ).set_branch("feat/x", "42")
    assert base.get_headers()["X-Talyvor-Session"] == "turn-0"

    nxt = base.set_session("turn-1", agent_name="planner")
    assert nxt.get_headers()["X-Talyvor-Session"] == "turn-1"
    assert nxt.get_headers()["X-Talyvor-Agent"] == "planner"
    # and the PR the parent carried is still there
    assert nxt.get_headers()["X-Talyvor-PR"] == "42"
