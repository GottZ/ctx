"""K0: ``hostcompat.grant_mcp_server`` (contract K-5, W108; DESIGN §13 "written with the public config writer").

Against the real ``hermes_cli.config`` of the upstream tree in the isolated ``HERMES_HOME``; the grant is proven
by the core's own gate reader (``PluginContext._mcp_allowlist``, ``hermes_cli/plugins_context.py:365-373``).
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import hostcompat
from ctx_handover.engine.errors import BadRequest, CapabilityMissing
from ctx_handover.tests.conftest import require_upstream


def _core_allowlist() -> list[str]:
    from hermes_cli.plugins_context import PluginContext

    return PluginContext._mcp_allowlist(hostcompat.PLUGIN_ID)


def _write_config(hermes_home, text: str) -> None:
    (hermes_home / "config.yaml").write_text(text, encoding="utf-8")


def _read_config(hermes_home) -> dict:
    from hermes_cli.config import read_user_config_raw

    return read_user_config_raw(hermes_home / "config.yaml")


def test_grant_writes_the_allowlist_the_core_reads(hermes_home):
    require_upstream()
    _write_config(
        hermes_home,
        "model:\n  default: m-1\n  provider: openrouter\n"
        "plugins:\n  entries:\n    ctx_handover:\n      mcp_allowlist: [other]\n"
        "      settings:\n        inbound: true\n",
    )
    assert _core_allowlist() == ["other"]
    hostcompat.grant_mcp_server("ctx")
    assert _core_allowlist() == ["other", "ctx"]
    cfg = _read_config(hermes_home)
    entry = cfg["plugins"]["entries"]["ctx_handover"]
    assert entry["mcp_allowlist"] == ["other", "ctx"] and entry["settings"] == {"inbound": True}
    assert cfg["model"]["default"] == "m-1" and cfg["model"]["provider"] == "openrouter"


def test_grant_is_idempotent_and_creates_a_missing_config(hermes_home):
    require_upstream()
    assert not (hermes_home / "config.yaml").exists()
    hostcompat.grant_mcp_server("ctx")
    assert _core_allowlist() == ["ctx"]
    before = (hermes_home / "config.yaml").read_bytes()
    hostcompat.grant_mcp_server("ctx")
    assert (hermes_home / "config.yaml").read_bytes() == before
    hostcompat.grant_mcp_server("ctx.archive-2")
    assert _core_allowlist() == ["ctx", "ctx.archive-2"]


@pytest.mark.parametrize("server", ["", "*", "a b", "../x", "x/y", "-x", ".x", "x" * 129, "ctx\n", None, 3])
def test_grant_refuses_names_that_are_no_server(server, hermes_home):
    require_upstream()
    with pytest.raises(BadRequest) as exc:
        hostcompat.grant_mcp_server(server)
    assert exc.value.details["field"] == "server"
    assert not (hermes_home / "config.yaml").exists()


def test_grant_refuses_on_a_managed_install(hermes_home, monkeypatch):
    require_upstream()
    import hermes_cli.config as core_config

    assert _core_allowlist() == []  # the home skeleton exists, as on any managed install
    monkeypatch.setattr(core_config, "is_managed", lambda: True)
    with pytest.raises(CapabilityMissing) as exc:
        hostcompat.grant_mcp_server("ctx")
    details = exc.value.details
    assert details["reason"] == "managed"
    assert details["command"] == "hermes config set plugins.entries.ctx_handover.mcp_allowlist '[\"ctx\"]'"
    assert not (hermes_home / "config.yaml").exists()


def test_grant_proves_the_write_by_reading_back(hermes_home, monkeypatch):
    """★ A save that does not land (the core strips an administrator-managed key, or a writer refuses silently)
    is reported as ``CapabilityMissing`` with ``reason=not_written`` and the manual command — never as success.

    Mutant: drop the read-back check after ``save_config``.
    """
    require_upstream()
    import hermes_cli.config as core_config

    _write_config(hermes_home, "plugins:\n  entries:\n    ctx_handover:\n      mcp_allowlist: [other]\n")
    monkeypatch.setattr(core_config, "save_config", lambda *a, **kw: None)
    with pytest.raises(CapabilityMissing) as exc:
        hostcompat.grant_mcp_server("ctx")
    assert exc.value.details["reason"] == "not_written"
    assert exc.value.details["command"].endswith("""'["other", "ctx"]'""")
    assert _core_allowlist() == ["other"]


def test_grant_maps_writer_errors(hermes_home, monkeypatch):
    require_upstream()
    import hermes_cli.config as core_config

    def boom(*a, **kw):
        raise RuntimeError("The config could not be read, so this change was not saved")

    monkeypatch.setattr(core_config, "save_config", boom)
    with pytest.raises(CapabilityMissing) as exc:
        hostcompat.grant_mcp_server("ctx")
    assert exc.value.details["reason"] == "write_failed" and "could not be read" in exc.value.details["error"]


def test_fake_grant_writes_the_fake_config(fake_host):
    hostcompat.grant_mcp_server("ctx")
    hostcompat.grant_mcp_server("ctx")
    assert hostcompat.load_config()["plugins"]["entries"]["ctx_handover"]["mcp_allowlist"] == ["ctx"]
    fake_host.mcp_grant_refusal = "managed"
    with pytest.raises(CapabilityMissing) as exc:
        hostcompat.grant_mcp_server("other")
    assert exc.value.details["reason"] == "managed"
    with pytest.raises(BadRequest):
        hostcompat.grant_mcp_server("*")
