"""Tests for the standalone inject_lens_headers helper."""

from __future__ import annotations

from talyvor_lens import inject_lens_headers


def test_inject_includes_authorization_and_workspace() -> None:
    headers = inject_lens_headers(api_key="tlv_secret", workspace_id="my-team")
    assert headers["Authorization"] == "Bearer tlv_secret"
    assert headers["X-Talyvor-Workspace"] == "my-team"


def test_inject_includes_all_provided_optional_fields() -> None:
    headers = inject_lens_headers(
        api_key="tlv_x",
        workspace_id="ws",
        team="core",
        feature="search",
        session_id="sess-1",
        agent_name="ranger",
        branch="feat/login",
        pr_number="42",
        commit="abc123",
        repository="org/repo",
    )
    assert headers["X-Talyvor-Team"] == "core"
    assert headers["X-Talyvor-Feature"] == "search"
    assert headers["X-Talyvor-Session"] == "sess-1"
    assert headers["X-Talyvor-Agent"] == "ranger"
    assert headers["X-Talyvor-Branch"] == "feat/login"
    assert headers["X-Talyvor-PR"] == "42"
    assert headers["X-Talyvor-Commit"] == "abc123"
    assert headers["X-Talyvor-Repository"] == "org/repo"


def test_inject_omits_empty_optional_fields() -> None:
    headers = inject_lens_headers(
        api_key="tlv_x",
        workspace_id="ws",
        team="",
        feature=None,
        session_id="",
    )
    # Empty/None values must be silently dropped — no empty header values.
    for forbidden in (
        "X-Talyvor-Team",
        "X-Talyvor-Feature",
        "X-Talyvor-Session",
        "X-Talyvor-Agent",
        "X-Talyvor-Branch",
    ):
        assert forbidden not in headers


def test_inject_preserves_existing_headers() -> None:
    existing = {"User-Agent": "MyApp/1.0", "Accept": "application/json"}
    headers = inject_lens_headers(existing, api_key="tlv_x")
    assert headers["User-Agent"] == "MyApp/1.0"
    assert headers["Accept"] == "application/json"
    assert headers["Authorization"] == "Bearer tlv_x"
    # Caller's dict must not be mutated — the helper copies its input.
    assert "Authorization" not in existing


def test_inject_default_workspace_when_empty() -> None:
    headers = inject_lens_headers(api_key="tlv_x", workspace_id="")
    assert headers["X-Talyvor-Workspace"] == "default"
