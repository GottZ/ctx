"""Layer R (red) for V08 (D part) of ``reports/L2-contracts.md``: offline capability has exactly one rule,
``session_import.offline_capable`` (contract X04); ``envfacts.is_offline_route`` delegates to it or is gone. So the
``EnvFacts.offline_capable`` a host reports for its own main route equals that rule for every configuration —
including the cases where the second rule decided differently (a local provider with a remote ``base_url``, a
``*.localhost`` name, a named ``providers.<name>`` entry on the LAN)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import envfacts, session_import
from ctx_handover.engine.model import Options
from ctx_handover.tests.fakes.planning_fake import host_facts

CONFIGS = {
    "local-provider-remote-url": {"model": {"provider": "ollama", "base_url": "https://api.example.net/v1"}},
    "dot-localhost": {"model": {"provider": "custom", "base_url": "http://gpu.localhost:8080/v1"}},
    "named-provider-on-the-lan": {
        "model": {"default": "m", "provider": "lan"},
        "providers": {"lan": {"base_url": "http://192.168.1.5:8000/v1"}},
    },
    "public-host": {"model": {"provider": "openrouter", "base_url": "https://openrouter.ai/api/v1"}},
    "loopback": {"model": {"provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}},
}


@pytest.mark.parametrize("name", sorted(CONFIGS))
def test_reported_offline_capability_is_the_one_rule(name, tmp_path, monkeypatch):
    config = CONFIGS[name]
    empty = tmp_path / "no-tools"
    empty.mkdir()
    monkeypatch.setenv("PATH", str(empty))  # no tool is probed; only the route matters here
    host = host_facts(hermes_home=str(tmp_path / "hermes-home"), document_cache=str(tmp_path / "docs"))
    facts = envfacts.probe(instance_name="x", options=Options(), host=host, config=config, scratch_dir=tmp_path)
    assert facts.offline_capable is session_import.offline_capable({}, config)
