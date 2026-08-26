"""Runtime negotiation of the checkpoint contract (plugin 0.4.3, wave A01-W9c).

These tests run against the real Hermes host on ``PYTHONPATH`` (the fork checkout is
a pure v2 host: constant 2, no tool-evidence helper) and simulate a v3 host via
monkeypatching. Set ``CTX_CHECKPOINT_PROVIDER_PATH`` to point the whole file at a
different provider source — that is how the red proof against the unchanged root
file is run.
"""

import importlib.util
import json
import logging
import os
import sys
from pathlib import Path

import pytest

import agent.conversation_compression as host_compression
import agent.memory_provider as host_contract

_PROVIDER_PATH = Path(
    os.environ.get("CTX_CHECKPOINT_PROVIDER_PATH")
    or Path(__file__).resolve().parents[1] / "__init__.py"
)
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_contract_negotiation", _PROVIDER_PATH
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(_MODULE)
CtxCheckpointMemoryProvider = _MODULE.CtxCheckpointMemoryProvider

_HOST_FEATURE = "_tool_evidence_for_pre_compress_memory"


def _dispatch_factory(calls, start=200):
    counter = start

    def dispatch(name, args, **kwargs):
        nonlocal counter
        calls.append((name, args))
        if name == "mcp__ctx__search":
            return json.dumps({"result": "[]"})
        block_id = f"019f6000-0000-7000-8000-{counter:012d}"
        counter += 1
        return json.dumps({"id": block_id})

    return dispatch


def _provider(calls):
    provider = CtxCheckpointMemoryProvider(dispatch=_dispatch_factory(calls))
    provider.initialize("session-negotiation", platform="cli")
    return provider


def _stores(calls):
    return [args for name, args in calls if name == "mcp__ctx__store"]


def _simulate_v3_host(monkeypatch, *, with_feature=True):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    if with_feature:
        monkeypatch.setattr(
            host_compression, _HOST_FEATURE, lambda messages: [], raising=False
        )
    elif hasattr(host_compression, _HOST_FEATURE):
        monkeypatch.delattr(host_compression, _HOST_FEATURE)


def _tool_evidence_warnings(caplog):
    return [
        record
        for record in caplog.records
        if record.levelno == logging.WARNING and "tool_evidence" in record.getMessage()
    ]


# --- host read path against the real fork host --------------------------------


def test_fork_host_is_a_pure_v2_host():
    # Pins the fixture the next test relies on; if the host on PYTHONPATH changes,
    # this fails first and names the reason.
    assert host_contract.PRE_COMPRESS_CHECKPOINT_API_VERSION == 2
    assert not hasattr(host_compression, _HOST_FEATURE)


def test_host_read_path_int_getattr_on_instance_negotiates_2_against_fork_host():
    provider = _provider([])

    # Exactly the host's read path (agent/memory_manager.py: int(getattr(...))).
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2
    assert isinstance(provider.pre_compress_checkpoint_api_version, int)
    assert isinstance(
        CtxCheckpointMemoryProvider.__dict__["pre_compress_checkpoint_api_version"],
        property,
    )
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert _MODULE.PLUGIN_CHECKPOINT_API_MAX == 2
    assert _MODULE.HOST_V3_FEATURE == _HOST_FEATURE


# --- simulated v3 host --------------------------------------------------------


def test_probe_reports_3_on_simulated_v3_host_but_plugin_declares_its_own_max(
    monkeypatch,
):
    _simulate_v3_host(monkeypatch)
    provider = _provider([])

    assert _MODULE._probe_host_checkpoint_api() == 3
    assert _MODULE.PLUGIN_CHECKPOINT_API_MAX == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_negotiation_flips_to_3_once_plugin_max_is_raised(monkeypatch):
    _simulate_v3_host(monkeypatch)
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    provider = _provider([])

    assert _MODULE._probe_host_checkpoint_api() == 3
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 3


def test_raised_plugin_max_alone_does_not_exceed_a_v2_host(monkeypatch):
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    provider = _provider([])

    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


def test_v3_constant_without_host_feature_probes_2(monkeypatch):
    _simulate_v3_host(monkeypatch, with_feature=False)

    assert _MODULE._probe_host_checkpoint_api() == 2


def test_v3_feature_present_but_not_callable_probes_2(monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    monkeypatch.setattr(host_compression, _HOST_FEATURE, "not-callable", raising=False)

    assert _MODULE._probe_host_checkpoint_api() == 2


def test_v1_host_probes_1_but_plugin_still_declares_2(monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 1)
    provider = _provider([])

    assert _MODULE._probe_host_checkpoint_api() == 1
    assert int(getattr(provider, "pre_compress_checkpoint_api_version", 1)) == 2


# --- probe robustness: never raises, falls back to 2 ---------------------------


def test_probe_falls_back_to_2_when_host_module_entry_is_none(monkeypatch):
    monkeypatch.setitem(sys.modules, "agent.memory_provider", None)

    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(_provider([]), "pre_compress_checkpoint_api_version", 1)) == 2


def test_probe_falls_back_to_2_when_host_module_name_does_not_exist(monkeypatch):
    monkeypatch.setattr(
        _MODULE, "_HOST_CONTRACT_MODULE", "agent.ctx_checkpoint_does_not_exist"
    )

    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_falls_back_to_2_when_feature_module_is_missing_on_v3_host(monkeypatch):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", 3)
    monkeypatch.setattr(
        _MODULE, "_HOST_FEATURE_MODULE", "agent.ctx_checkpoint_does_not_exist"
    )

    assert _MODULE._probe_host_checkpoint_api() == 2


@pytest.mark.parametrize("bad_value", ["3", 3.0, None, True, [3]])
def test_probe_falls_back_to_2_on_non_int_constant(monkeypatch, bad_value):
    monkeypatch.setattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION", bad_value)

    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_falls_back_to_2_when_constant_is_missing(monkeypatch):
    monkeypatch.delattr(host_contract, "PRE_COMPRESS_CHECKPOINT_API_VERSION")

    assert _MODULE._probe_host_checkpoint_api() == 2


def test_probe_swallows_exception_raised_during_import(monkeypatch):
    class _Exploding:
        @staticmethod
        def find_spec(name, path=None, target=None):
            if name == "agent.memory_provider":
                raise RuntimeError("simulated import failure")
            return None

    monkeypatch.delitem(sys.modules, "agent.memory_provider")
    monkeypatch.setattr(sys, "meta_path", [_Exploding()] + sys.meta_path)

    with pytest.raises(RuntimeError):
        importlib.import_module("agent.memory_provider")
    assert _MODULE._probe_host_checkpoint_api() == 2
    assert int(getattr(_provider([]), "pre_compress_checkpoint_api_version", 1)) == 2


# --- tool_evidence keyword tolerance ------------------------------------------


def test_tool_evidence_kwarg_is_accepted_and_labelled_received_unrendered(caplog):
    calls = []
    provider = _provider(calls)
    evidence = [{"ordinal": 3, "tool": "read_file", "summary": "read README.md"}]

    with caplog.at_level(logging.WARNING):
        frame = provider.on_pre_compress(
            [{"role": "user", "content": "evidence with a tool index"}],
            tool_evidence=evidence,
            future_keyword="ignored",
        )

    stores = _stores(calls)
    assert len(stores) == 3
    assert all(
        args["metadata"]["tool_index_status"] == "received-unrendered" for args in stores
    )
    assert "read README.md" not in stores[0]["content"]
    assert len(frame) < 1_000


def test_tool_evidence_warning_fires_once_per_instance(caplog):
    calls = []
    provider = _provider(calls)

    with caplog.at_level(logging.WARNING):
        provider.on_pre_compress(
            [{"role": "user", "content": "first"}], tool_evidence=[{"ordinal": 1}]
        )
        provider.on_pre_compress(
            [{"role": "user", "content": "second"}], tool_evidence=[]
        )

    assert len(_tool_evidence_warnings(caplog)) == 1
    assert all(
        args["metadata"]["tool_index_status"] == "received-unrendered"
        for args in _stores(calls)
    )

    with caplog.at_level(logging.WARNING):
        _provider([]).on_pre_compress(
            [{"role": "user", "content": "third"}], tool_evidence=[{"ordinal": 2}]
        )

    assert len(_tool_evidence_warnings(caplog)) == 2


def test_without_tool_evidence_status_is_absent_and_silent(caplog):
    calls = []
    provider = _provider(calls)

    with caplog.at_level(logging.WARNING):
        provider.on_pre_compress([{"role": "user", "content": "plain prose"}])

    stores = _stores(calls)
    assert len(stores) == 3
    assert all(args["metadata"]["tool_index_status"] == "absent" for args in stores)
    assert _tool_evidence_warnings(caplog) == []


# --- negotiated contract in metadata and head body ----------------------------


def test_metadata_carries_negotiated_contract_on_part_manifest_and_head():
    calls = []
    provider = _provider(calls)

    provider.on_pre_compress([{"role": "user", "content": "negotiated contract"}])

    part, manifest, head = _stores(calls)
    assert part["title"].endswith("part 001 of 001")
    assert manifest["title"].endswith("manifest")
    assert head["title"] == "Compaction checkpoint head session-negotiation"
    for args in (part, manifest, head):
        metadata = args["metadata"]
        assert metadata["host_checkpoint_api"] == 2
        assert metadata["checkpoint_api_version"] == 2
        assert metadata["tool_index_status"] == "absent"
        assert metadata["checkpoint_api_version"] != 1
    assert "- Checkpoint API version: 2\n" in head["content"]
    assert "Checkpoint API version: 1" not in head["content"]


def test_head_reflects_negotiated_3_on_v3_host_with_raised_plugin_max(monkeypatch):
    _simulate_v3_host(monkeypatch)
    monkeypatch.setattr(_MODULE, "PLUGIN_CHECKPOINT_API_MAX", 3)
    calls = []
    provider = _provider(calls)

    provider.on_pre_compress([{"role": "user", "content": "v3 negotiated"}])

    part, manifest, head = _stores(calls)
    for args in (part, manifest, head):
        assert args["metadata"]["host_checkpoint_api"] == 3
        assert args["metadata"]["checkpoint_api_version"] == 3
    assert "- Checkpoint API version: 3\n" in head["content"]
