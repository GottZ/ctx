"""``plugins.isolation: host`` as this Hermes handles it, and what the plugin's status says about it.

Under host isolation Hermes loads third-party plugins in a plugin host process: the plugin's own check (A1)
refuses there, so it registers nothing, and Hermes skips ``register_cli_command`` in the host, so
``hermes ctx-discord-policy`` does not exist either (the README says both). Should a Hermes ever wire plugin
commands from the host, the first test fails, and the CLI's own isolation line is what users see then.
"""

from __future__ import annotations

import argparse
import io
from pathlib import Path


def test_host_isolation_runs_plugins_in_the_host_and_skips_their_cli_commands(hermes, tmp_path):
    from fakes.hermes_homes import install_plugin
    from hermes_cli.plugin_isolation import HOST_SKIPPED_CTX_METHODS
    from hermes_cli.plugins import get_plugin_manager
    from hermes_cli.plugins_discovery import collect_directory_manifests
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    assert "register_cli_command" in HOST_SKIPPED_CTX_METHODS
    home = tmp_path / "isolated"
    install_plugin(home / "plugins" / "ctx_discord_policy")
    (home / "config.yaml").write_text("plugins:\n  enabled: [ctx_discord_policy]\n  isolation: host\n",
                                      encoding="utf-8")
    token = set_hermes_home_override(str(home))
    try:
        manifest = next(m for m in collect_directory_manifests() if m.name == "ctx_discord_policy")
        assert get_plugin_manager()._runs_in_plugin_host(manifest)
    finally:
        reset_hermes_home_override(token)


def test_the_status_reads_the_isolation_mode_of_the_profile(hermes, anchor, tmp_path):
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    home = tmp_path / "profile-home"
    home.mkdir()
    cli = anchor.mod("cli")
    parser = argparse.ArgumentParser()
    cli.setup(parser)
    texts = {}
    for mode in ("host", "in_process"):
        (home / "config.yaml").write_text(f"plugins:\n  isolation: {mode}\n", encoding="utf-8")
        token = set_hermes_home_override(str(home))
        try:
            assert anchor.mod("hermes").isolation_mode()[0] == mode
            out = io.StringIO()
            assert cli.run(parser.parse_args(["status"]), home=home, out=out, err=io.StringIO()) == 0
            texts[mode] = out.getvalue()
        finally:
            reset_hermes_home_override(token)
    assert texts["host"].startswith("Not active in this profile: plugins.isolation is host"), texts["host"]
    assert "Not active" not in texts["in_process"]


def test_the_settings_page_reads_the_isolation_mode_of_the_profile(hermes, anchor, tmp_path, monkeypatch):
    """Under host isolation Hermes still mounts the page's backend (in the plugin host, where
    HERMES_PLUGIN_HOST_PROCESS is set): the page is the one surface left, and it says the plugin is not active."""
    from fakes.dashboard import FakeBackend
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    routes = anchor.mod("web.routes")
    home = tmp_path / "page-home"
    home.mkdir()

    def page(path):
        backend = FakeBackend()
        backend.hermes = anchor.mod("hermes")
        req = routes.WebRequest(method="GET", path=path, query={}, body=None, home=home,
                                plugin_dir=Path(anchor.entry.__file__).parent,
                                process="dashboard", surface="dashboard", backend=backend)
        return routes.handle(req).body

    seen = {}
    for mode in ("in_process", "host"):
        (home / "config.yaml").write_text(f"plugins:\n  isolation: {mode}\n", encoding="utf-8")
        token = set_hermes_home_override(str(home))
        try:
            seen[mode] = (page("/status")["isolation"], page("/overview")["protection"]["isolation"])
        finally:
            reset_hermes_home_override(token)
    assert all(i["allows_plugin"] is True for i in seen["in_process"]), seen
    assert all(i["mode"] == "host" and i["allows_plugin"] is False for i in seen["host"]), seen
    (home / "config.yaml").write_text("plugins:\n  isolation: in_process\n", encoding="utf-8")
    monkeypatch.setenv("HERMES_PLUGIN_HOST_PROCESS", "1")      # inside the plugin host child
    token = set_hermes_home_override(str(home))
    try:
        assert page("/status")["isolation"]["allows_plugin"] is False
    finally:
        reset_hermes_home_override(token)
