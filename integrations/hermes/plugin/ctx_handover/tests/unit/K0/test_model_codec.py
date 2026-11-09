"""K0: data model, wire codec, bounded JSON, ids, version source and the error envelope (TESTPLAN §2.1)."""

from __future__ import annotations

import dataclasses
import enum
import json
import re
import types
import typing
from datetime import datetime

import pytest

import ctx_handover.engine as engine_pkg
from ctx_handover.engine import errors, ids, jsonutil, model
from ctx_handover.engine.contracts import (
    BlockSink,
    BlockSource,
    BusyReportLike,
    GitProbe,
    JobContext,
    Reporter,
    SourceAPI,
    TargetAPI,
)
from ctx_handover.engine.errors import BadRequest, HandoverError
from ctx_handover.tests.conftest import PLUGIN_ROOT

MODEL_CLASSES = sorted(
    (obj for obj in vars(model).values() if isinstance(obj, type) and dataclasses.is_dataclass(obj)),
    key=lambda c: c.__name__,
)


def _sample(tp, depth: int = 0):
    """A non-default sample value for a type hint (deterministic)."""
    origin, args = typing.get_origin(tp), typing.get_args(tp)
    if origin is typing.Union or origin is types.UnionType:
        options = [a for a in args if a is not type(None)]
        return _sample(options[0], depth)
    if tp is type(None) or tp is None:
        return None
    if tp is bool:
        return True
    if tp is int:
        return 7
    if tp is float:
        return 1.5
    if tp is str:
        return "ä/x"
    if isinstance(tp, type) and issubclass(tp, enum.Enum):
        return list(tp)[-1]
    if isinstance(tp, type) and dataclasses.is_dataclass(tp):
        return _instance(tp, depth + 1)
    if tp is tuple:
        return (3, "rev", (1, 2))
    if origin is tuple:
        if len(args) == 2 and args[1] is Ellipsis:
            return (_sample(args[0], depth),)
        return tuple(_sample(a, depth) for a in args)
    if origin is frozenset:
        return frozenset({_sample(args[0], depth)})
    if tp is dict:
        return {"a": 1, "b": [1, "x"], "c": {"d": None}}
    if origin is dict:
        return {"k": _sample(args[1], depth)}
    raise AssertionError(f"no sample for {tp!r}")


def _instance(cls, depth: int = 0):
    hints = typing.get_type_hints(cls)
    return cls(**{f.name: _sample(hints[f.name], depth) for f in dataclasses.fields(cls)})


@pytest.mark.parametrize("cls", MODEL_CLASSES, ids=lambda c: c.__name__)
def test_codec_roundtrip_all_models(cls):
    """Every model dataclass survives to_wire → JSON text → loads_bounded → from_wire unchanged."""
    obj = _instance(cls)
    wire = jsonutil.to_wire(obj)
    text = jsonutil.canonical_bytes(wire)
    back = jsonutil.from_wire(cls, jsonutil.loads_bounded(text, max_bytes=1 << 20))
    assert back == obj
    assert jsonutil.canonical_bytes(back) == text


def test_codec_defaults_roundtrip_and_canonical_form():
    o = model.Options()
    assert jsonutil.from_wire(model.Options, jsonutil.to_wire(o)) == o
    raw = jsonutil.canonical_bytes({"b": 1, "a": [frozenset({"y", "x"})], "ü": model.Right.PULL})
    assert raw == '{"a":[["x","y"]],"b":1,"ü":"pull"}'.encode()
    assert jsonutil.doc_id({"a": 1}) == jsonutil.sha256_hex(b'{"a":1}')
    with pytest.raises(ValueError):
        jsonutil.canonical_bytes({"x": float("nan")})


def test_codec_ignores_unknown_fields():
    """Additive rule: unknown keys are ignored, missing keys take the defaults (INTERFACES §3.1)."""
    data = {"step": "packing", "bytes_done": 5, "future_field": {"x": 1}}
    assert jsonutil.from_wire(model.Progress, data) == model.Progress(step="packing", bytes_done=5)
    hello = jsonutil.from_wire(
        model.Hello,
        {"protocol": "ctxh", "majors": [1], "minor": 0, "instance_id": "I", "listener_pubkey": "K", "new": 1},
    )
    assert hello.limits == model.Limits() and hello.capabilities == frozenset()


@pytest.mark.parametrize(
    ("cls", "data", "field"),
    [
        (model.Progress, {"step": 3}, "step"),
        (model.Progress, {"step": "s", "bytes_done": True}, "bytes_done"),
        (model.Progress, {"step": "s", "bytes_done": 1.0}, "bytes_done"),
        (model.Progress, {}, "step"),
        (model.PeerInfo, {"peer_id": "p", "rights": ["push", "admin"]}, "rights[1]"),
        (
            model.Hello,
            {"protocol": "p", "majors": "1", "minor": 0, "instance_id": "i", "listener_pubkey": "k"},
            "majors",
        ),
        (
            model.TargetDecision,
            {"item_id": "i", "dest": "/d", "mode": "fresh", "reason": "r", "git_seed": "x"},
            "git_seed",
        ),
        (model.OpStatus, {"op_id": "o", "state": "done", "progress": {"step": None}}, "progress.step"),
        (
            model.Refinement,
            {"add": [["a"]], "drop": [], "tools_needed": [], "env_vars_needed": [], "notes": ""},
            "add[0]",
        ),
        (model.FinalPlan, {"plan_id": "x"}, "plan"),
    ],
)
def test_codec_rejects_wrong_types(cls, data, field):
    """Wrong types raise BadRequest naming the field path."""
    with pytest.raises(BadRequest) as info:
        jsonutil.from_wire(cls, data)
    assert info.value.details["field"] == field
    assert info.value.code == "bad_request"


def test_codec_float_accepts_int_but_int_rejects_float():
    assert jsonutil.from_wire(model.Peer, _peer_wire(created_at=3)).created_at == 3.0


def _peer_wire(**over):
    data = {"name": "n", "peer_id": "p", "direction": "dial", "url": None, "pubkey": "k", "rights": ["pull"]}
    data.update(over)
    return data


def test_loads_bounded_depth_and_size(monkeypatch):
    """★ Depth > 64 or size over the cap is rejected before ``json.loads`` runs.

    Red proof: mutant = remove the ``_check_depth(data, max_depth)`` call in ``jsonutil.loads_bounded`` →
    ``Failed: DID NOT RAISE BadRequest`` (65 levels parse; the ``json.loads`` spy guards the size path).
    """
    calls = []
    real_loads = json.loads

    def spy(*a, **kw):
        calls.append(1)
        return real_loads(*a, **kw)

    monkeypatch.setattr(jsonutil.json, "loads", spy)
    deep = b"[" * 65 + b"]" * 65
    with pytest.raises(BadRequest) as info:
        jsonutil.loads_bounded(deep, max_bytes=1 << 20)
    assert info.value.details["reason"] == "depth"
    assert calls == [], "json.loads called on rejected input"
    with pytest.raises(BadRequest) as info:
        jsonutil.loads_bounded(b'{"a": "' + b"x" * 100 + b'"}', max_bytes=50)
    assert info.value.details["reason"] == "too_large"
    assert calls == [], "json.loads called on rejected input"
    assert jsonutil.loads_bounded(b"[" * 64 + b"]" * 64, max_bytes=1 << 20) is not None
    assert calls == [1]


def test_loads_bounded_ignores_brackets_inside_strings_and_escapes():
    text = json.dumps({"s": "[" * 200 + '\\"{' * 100, "t": ["]"] * 3}).encode()
    assert jsonutil.loads_bounded(text, max_bytes=1 << 20)["t"] == ["]", "]", "]"]


@pytest.mark.parametrize(
    "raw",
    [b'{"a":1,"a":2}', b'{"a": NaN}', b'{"a": Infinity}', b"\xff\xfe", b"{", b'{"a":' + b"9" * 5000 + b"}"],
)
def test_loads_bounded_rejects_invalid(raw):
    with pytest.raises(BadRequest) as info:
        jsonutil.loads_bounded(raw, max_bytes=1 << 20)
    assert info.value.details["reason"] == "invalid"


def test_mint_session_id_format():
    sid = ids.mint_session_id()
    assert re.fullmatch(r"\d{8}_\d{6}_[0-9a-f]{6}", sid)
    assert ids.mint_session_id(datetime(2026, 11, 9, 1, 2, 3)).startswith("20261109_010203_")


def test_ulid_and_crockford():
    a, b = ids.ulid(), ids.ulid()
    assert re.fullmatch(r"[0-9A-HJKMNP-TV-Z]{26}", a) and a != b
    assert a[:10] <= b[:10]
    assert len(ids.crockford(10)) == 16 and len(ids.crockford(5)) == 8
    assert ids.short("abcdefgh") == "cdefgh"


def test_version_single_source():
    """``VERSION``, ``plugin.yaml`` and ``engine.PLUGIN_VERSION`` agree [BX L9]."""
    version = (PLUGIN_ROOT / "VERSION").read_text(encoding="utf-8").strip()
    manifest = (PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8")
    match = re.search(r"^version:\s*['\"]?([^'\"\s]+)", manifest, re.MULTILINE)
    assert match is not None
    assert match.group(1) == version == engine_pkg.PLUGIN_VERSION
    assert re.fullmatch(r"\d+\.\d+\.\d+", version)


def test_engine_package_constants():
    assert engine_pkg.WIRE_MAJOR == 1 and isinstance(engine_pkg.WIRE_MINOR, int)
    assert "session.v1" in engine_pkg.CAPABILITIES_V1
    with pytest.raises(AttributeError):
        engine_pkg.NOT_A_CONSTANT  # noqa: B018


# --------------------------------------------------------------------------------------------------------
# Error envelope (DESIGN §4.4)
# --------------------------------------------------------------------------------------------------------

DESIGN_ERROR_TABLE = {
    "inbound_disabled": (404, False),
    "unauthorized": (401, False),
    "forbidden": (403, False),
    "version_unsupported": (400, False),
    "capability_missing": (400, False),
    "bad_request": (400, False),
    "payload_too_large": (413, False),
    "not_found": (404, False),
    "job_conflict": (409, False),
    "invalid_state": (409, False),
    "session_busy": (409, True),
    "wait_timeout": (409, True),
    "already_handing_over": (409, False),
    "stale_session": (409, False),
    "stale_worktree": (409, False),
    "restart_limit": (409, False),
    "policy_denied": (422, False),
    "conflict": (409, False),
    "insufficient_storage": (507, False),
    "block_corrupt": (422, True),
    "verify_failed": (422, False),
    "import_failed": (500, False),
    "rate_limited": (429, True),
    "pair_invalid": (401, False),
    "continue_refused": (403, False),
    "internal": (500, True),
}
CLIENT_ONLY = {
    "gate_rejected",
    "listener_missing",
    "peer_unreachable",
    "bad_response_signature",
    "https_required",
    "lease_lost",
    "cancelled",
    "in_doubt",
    "identity_lost",
    "env_not_writable",
}


def test_error_codes_match_design_table():
    assert frozenset(DESIGN_ERROR_TABLE) == errors.WIRE_CODES
    assert frozenset(CLIENT_ONLY) == errors.CLIENT_CODES
    for code, (status, retryable) in DESIGN_ERROR_TABLE.items():
        klass = errors.error_class(code)
        assert klass is not None and klass.http_status == status, code
        assert klass("x").retryable is retryable, code
    assert errors.error_class("peer_unreachable")("x").retryable is True


def test_error_wire_roundtrip_and_detail_fields():
    err = errors.InsufficientStorage("disk full", need_bytes=10, free_bytes=2, path="/x")
    env = err.to_wire()
    assert env == {
        "error": {
            "code": "insufficient_storage",
            "message": "disk full",
            "retryable": False,
            "details": {"need_bytes": 10, "free_bytes": 2, "path": "/x"},
        }
    }
    back = HandoverError.from_wire(507, env)
    assert type(back) is errors.InsufficientStorage and back.details == env["error"]["details"]
    assert str(back) == "insufficient_storage: disk full"


@pytest.mark.parametrize("code", sorted(CLIENT_ONLY | {"made_up", "x" * 500}))
def test_error_from_wire_never_trusts_client_only_or_unknown_codes(code):
    back = HandoverError.from_wire(409, {"error": {"code": code, "message": "m", "retryable": False}})
    assert type(back) is HandoverError and back.code == "internal"
    assert back.details["peer_code"] == code[:64]
    assert back.retryable is False


def test_error_from_wire_caps_peer_text_and_ignores_peer_retryable_for_known_codes():
    huge = {"k": "v" * 5000}
    back = HandoverError.from_wire(
        422, {"error": {"code": "verify_failed", "message": "m" * 5000, "retryable": True, "details": huge}}
    )
    assert type(back) is errors.VerifyFailed and back.retryable is False
    assert len(back.message) == errors.PEER_TEXT_CAP and back.details == {"truncated": True}
    bare = HandoverError.from_wire(502, {"detail": "Bad Gateway"})
    assert bare.code == "internal" and bare.retryable is True and bare.details["status"] == 502
    assert HandoverError.from_wire(404, "not json").retryable is False


def test_error_to_peer_redacts_caps_and_hides_client_only_codes():
    def redact(text: str) -> str:
        return text.replace("SECRET", "***")

    env = errors.LeaseLost("lost SECRET lease").to_peer(redact)
    assert env["error"]["code"] == "internal" and "SECRET" not in env["error"]["message"]
    env = errors.PolicyDenied("SECRET in path", items=["SECRET"] * 400).to_peer(redact)
    assert env["error"]["code"] == "policy_denied" and env["error"]["details"] == {"truncated": True}
    env = errors.ImportFailed("x", hermes_error="ValueError: SECRET").to_peer(redact)
    assert env["error"]["details"] == {"hermes_error": "ValueError: ***"}


# --------------------------------------------------------------------------------------------------------
# Protocols
# --------------------------------------------------------------------------------------------------------


def test_protocols_are_runtime_checkable_and_complete():
    class Rep:
        def progress(self, p):
            pass

        def info(self, text):
            pass

        def warn(self, text):
            pass

    assert isinstance(Rep(), Reporter)
    job = JobContext("j", model.Mode.PUSH, None, model.Options(), 1, Rep(), lambda: False, 0.0)
    assert jsonutil.to_wire(job.options) == jsonutil.to_wire(model.Options())
    source_methods = {
        "hello",
        "list_sessions",
        "facts",
        "start_plan",
        "lock",
        "heartbeat",
        "start_export",
        "start_pack",
        "start_prepare",
        "start_retire",
        "start_release",
        "op_status",
        "status",
        "blocks",
    }
    target_methods = {
        "hello",
        "open_job",
        "facts",
        "negotiate",
        "start_stage_context",
        "start_prepare",
        "start_apply_delta",
        "start_reset_session",
        "start_commit",
        "continue_session",
        "abort",
        "op_status",
        "status",
        "blocks",
    }
    assert {m for m in vars(SourceAPI) if not m.startswith("_")} >= source_methods
    assert {m for m in vars(TargetAPI) if not m.startswith("_")} >= target_methods
    for proto in (SourceAPI, TargetAPI, BlockSource, BlockSink, GitProbe, Reporter, BusyReportLike):
        for name, fn in vars(proto).items():
            if callable(fn) and not name.startswith("_"):
                assert fn.__doc__, f"{proto.__name__}.{name} lacks a docstring"
