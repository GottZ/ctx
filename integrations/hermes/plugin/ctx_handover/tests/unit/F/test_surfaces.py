"""Cluster F — parsing, settings, user-facing texts (DESIGN §11.6, §14.1, §14.4; TESTPLAN §2.7)."""

from __future__ import annotations

from pathlib import Path

import pytest

from ctx_handover.engine import surfaces
from ctx_handover.engine.errors import (
    AlreadyHandingOver,
    CapabilityMissing,
    ContinueRefused,
    Forbidden,
    GateRejected,
    HandoverError,
    HttpsRequired,
    InDoubt,
    InsufficientStorage,
    ListenerMissing,
    PairInvalid,
    PayloadTooLarge,
    PeerUnreachable,
    PolicyDenied,
    RestartLimit,
    SessionBusy,
    Unauthorized,
    VerifyFailed,
    WaitTimeout,
)
from ctx_handover.engine.model import (
    CommitAck,
    ContinueResult,
    DestMode,
    GateMode,
    Mode,
    Options,
    PathMapEntry,
    Peer,
    Right,
)

PLUGIN_ROOT = Path(surfaces.__file__).resolve().parents[1]

# DESIGN §14.4, transcribed literally (key: (type, default)); secrets carry their env name instead of a default.
DESIGN_14_4 = {
    "inbound": ("str", "auto"),
    "listen_url": ("str", ""),
    "peer_gate": ("str", "session_token"),
    "peer_token": ("secret", "CTX_HANDOVER_PEER_TOKEN"),
    "peer_username": ("str", ""),
    "peer_password": ("secret", "CTX_HANDOVER_PEER_PASSWORD"),
    "peer_extra_headers": ("dict", {}),
    "workspace_root": ("str", "~/hermes-handover"),
    "allow_roots": ("list", ["~/hermes-handover"]),
    "export_roots": ("list", []),
    "max_session_mb": ("int", 512),
    "max_file_mb": ("int", 100),
    "max_extra_dir_mb": ("int", 256),
    "max_hermes_file_mb": ("int", 64),
    "max_files": ("int", 200000),
    "max_untracked_files": ("int", 200000),
    "max_total_gb": ("float", 10),
    "confirm_above_mb": ("int", 500),
    "max_request_mb": ("int", 8),
    "block_cache_gb": ("float", 5),
    "block_cache_days": ("int", 3),
    "staging_ttl_hours": ("int", 24),
    "lease_ttl_s": ("int", 120),
    "participant_idle_timeout_min": ("int", 15),
    "stall_timeout_min": ("int", 30),
    "wait_timeout_min": ("int", 30),
    "refine": ("str", "auto"),
    "refine_timeout_s": ("int", 45),
    "refine_history_chars": ("int", 60000),
    "refine_side_question_max_tokens": ("int", 200000),
    "refine_auto_include": ("bool", False),
    "extra_paths": ("str", "ask"),
    "include_hermes_files": ("bool", True),
    "model_mapping": ("str", "auto"),
    "allow_remote_continue": ("bool", False),
    "allow_agent_start": ("bool", False),
    "accept_offers": ("str", "ask"),
    "extra_ignore": ("list", []),
    "tool_probe": ("list", []),
    "ctx_persist": ("str", "auto"),
    "ctx_category": ("str", "handover"),
}
DESIGN_CHOICES = {
    "inbound": ["auto", "on", "off"],
    "peer_gate": ["session_token", "bearer", "basic_login"],
    "refine": ["auto", "side_question", "llm", "off"],
    "extra_paths": ["ask", "auto", "off"],
    "model_mapping": ["auto", "target_default"],
    "accept_offers": ["ask", "off"],
    "ctx_persist": ["auto", "off"],
}
NOT_IN_SCHEMA = {"mcp_allowlist", "allow_gateway_injection", "granted_capabilities", "operator_api"}


def _manifest() -> dict:
    import hermes_yaml
    from hermes_cli.plugins_manifest import PluginManifest  # noqa: F401  (real parser is importable)

    return hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))


def test_config_schema_matches_design_table():
    """``plugin.yaml`` ``config_schema`` and ``surfaces.SETTINGS_SCHEMA`` both equal DESIGN §14.4 (keys, types,
    defaults, choices, secret env names); operator keys stay out of the form."""
    schema = _manifest()["config_schema"]
    assert set(schema) == set(DESIGN_14_4)
    assert set(surfaces.SETTINGS_SCHEMA) == set(DESIGN_14_4)
    for key, (typ, default) in DESIGN_14_4.items():
        spec = schema[key]
        assert spec["type"] == typ, key
        mirror = surfaces.SETTINGS_SCHEMA[key]
        assert mirror["type"] == typ, key
        if typ == "secret":
            assert spec["env"] == default and mirror["env"] == default, key
            assert "default" not in spec, key
        else:
            assert spec["default"] == default, key
            assert mirror["default"] == default, key
        assert spec.get("choices") == DESIGN_CHOICES.get(key), key
        assert list(mirror.get("choices") or []) == (DESIGN_CHOICES.get(key) or []), key
    assert not (NOT_IN_SCHEMA & set(schema))


def test_manifest_declares_floor_tools_hooks_and_capability():
    data = _manifest()
    assert data["requires_hermes"] == ">=0.22.6"
    assert data["kind"] == "standalone"
    assert data["provides_tools"] == ["handover"]
    assert set(data["provides_hooks"]) == {"pre_llm_call", "pre_tool_call", "post_llm_call", "post_compaction"}
    assert data["capabilities"] == ["llm.side_question"]


def test_version_single_source():
    """BX L9: ``VERSION`` is the only version source; ``plugin.yaml`` and the engine agree with it."""
    from ctx_handover.engine import PLUGIN_VERSION

    version = (PLUGIN_ROOT / "VERSION").read_text(encoding="utf-8").strip()
    assert _manifest()["version"] == version == PLUGIN_VERSION


def test_settings_defaults_and_invalid_values():
    cfg = {
        "plugins": {
            "entries": {
                "ctx_handover": {
                    "settings": {
                        "inbound": False,  # YAML `off`
                        "max_total_gb": 2.5,
                        "max_files": True,  # bool is not an int
                        "refine": "turbo",  # not a choice
                        "allow_roots": ["~/a", 3],
                        "peer_extra_headers": {"X-A": "ENV_A", "bad header": "B"},
                    }
                }
            }
        }
    }
    s = surfaces.settings_from_config(cfg)
    assert s["inbound"] == "off"
    assert s["max_total_gb"] == 2.5
    assert s["max_files"] == 200000
    assert s["refine"] == "auto"
    assert s["allow_roots"] == ["~/a"]
    assert s["peer_extra_headers"] == {"X-A": "ENV_A"}
    assert "peer_token" not in s and "peer_password" not in s
    assert surfaces.settings_from_config({})["workspace_root"] == "~/hermes-handover"
    assert surfaces.settings_from_config(None)["ctx_category"] == "handover"  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("raw", "verb", "check"),
    [
        ("", "help", {}),
        ("to server", "to", {"peer": "server"}),
        (
            "to server --dest /x --map /a=/b --map /c=/d --include ~/notes.md --include-secret .env --exclude '*.log' "
            "--no-workspace --model qwen3 --wait --force --dry-run --yes --max-total-gb 2",
            "to",
            {
                "dest": "/x",
                "maps": [("/a", "/b"), ("/c", "/d")],
                "include": ["~/notes.md"],
                "include_secret": [".env"],
                "exclude": ["*.log"],
                "no_workspace": True,
                "model": "qwen3",
                "wait": True,
                "force": True,
                "dry_run": True,
                "yes": True,
                "max_total_gb": 2.0,
            },
        ),
        ("to server --continue", "to", {"continue": surfaces.DEFAULT_CONTINUE}),
        ('to server --continue "run the tests" --yes', "to", {"continue": "run the tests", "yes": True}),
        ("to server --continue=go --wait", "to", {"continue": "go", "wait": True}),
        ("to server --continue --wait", "to", {"continue": surfaces.DEFAULT_CONTINUE, "wait": True}),
        ("take", "take", {"positionals": []}),
        ("take server", "take", {"positionals": ["server"]}),
        ("take server 20261109_141003_a1b2c3", "take", {"positionals": ["server", "20261109_141003_a1b2c3"]}),
        ("take --latest", "take", {"latest": True}),
        ("take server --offer 01JCOFFER", "take", {"offer": "01JCOFFER"}),
        ("export", "export", {"path": None}),
        (
            "export ~/x.ctxh.tar --keep --for laptop --dir",
            "export",
            {"path": "~/x.ctxh.tar", "keep": True, "for_peer": "laptop", "as_dir": True},
        ),
        (
            "import ~/b.ctxh.tar --dest ~/w --trust-unsigned --again --continue",
            "import",
            {
                "bundle": "~/b.ctxh.tar",
                "dest": "~/w",
                "trust_unsigned": True,
                "again": True,
                "continue": surfaces.DEFAULT_CONTINUE,
            },
        ),
        ("status", "status", {"job": None}),
        ("status 01JCJOB", "status", {"job": "01JCJOB"}),
        ("peers", "peers", {"sub": "list"}),
        ("peers revoke laptop", "peers", {"sub": "revoke", "names": ["laptop"]}),
        ("peers remove server", "peers", {"sub": "remove", "names": ["server"]}),
        ("peers rename a b", "peers", {"sub": "rename", "names": ["a", "b"]}),
        ("peers show a", "peers", {"sub": "show", "names": ["a"]}),
        ("pair", "pair", {"pair_string": None}),
        (
            "pair --name laptop --rights push,pull --ttl 10m",
            "pair",
            {"name": "laptop", "ttl_s": 600, "rights": frozenset({Right.PUSH, Right.PULL})},
        ),
        ("pair --rights pull --ttl 45s", "pair", {"rights": frozenset({Right.PULL}), "ttl_s": 45}),
        (
            "pair ctxh-pair:abc --name server --gate basic_login --allow-insecure",
            "pair",
            {"pair_string": "ctxh-pair:abc", "name": "server", "gate": GateMode.BASIC_LOGIN, "allow_insecure": True},
        ),
        ("reclaim", "reclaim", {"session": None, "force": False}),
        ("reclaim 20261109_141003_a1b2c3 --force", "reclaim", {"session": "20261109_141003_a1b2c3", "force": True}),
        ("cancel", "cancel", {"job": None}),
        ("resume 01JCJOB", "resume", {"job": "01JCJOB"}),
        ("setup", "setup", {}),
        ("doctor", "doctor", {}),
        ("identity", "identity", {"sub": "show"}),
        ("identity rotate", "identity", {"sub": "rotate"}),
        ("gc", "gc", {"job": None, "all": False}),
        ("gc --job 01JCJOB", "gc", {"job": "01JCJOB"}),
        ("gc --all", "gc", {"all": True}),
        ("logs 01JCJOB", "logs", {"job": "01JCJOB"}),
        ("TO server", "to", {"peer": "server"}),
    ],
)
def test_parse_all_verbs(raw, verb, check):
    parsed = surfaces.parse(raw)
    assert parsed.errors == (), parsed.errors
    assert parsed.verb == verb
    for key, value in check.items():
        assert parsed.args.get(key) == value, key


@pytest.mark.parametrize(
    "raw",
    [
        "fly away",
        "to",
        "to a b",
        "to server --bogus",
        "to server --dest",
        "to server --map nope",
        "to server --max-total-gb lots",
        "import",
        "logs",
        "pair --ttl 2h",
        "pair --ttl soon",
        "pair --rights admin",
        "pair --gate none",
        "pair not-a-pair-string",
        "gc --job a --all",
        "take --latest --offer x",
        "to server 'unbalanced",
        "peers frobnicate x",
        "peers revoke",
        "identity explode",
        "status a b",
        "setup now",
    ],
)
def test_parse_errors_never_exit(raw):
    """TESTPLAN ``test_parse_all_verbs_and_errors``: every malformed input yields errors, never ``SystemExit``."""
    parsed = surfaces.parse(raw)
    assert parsed.errors, raw


def test_no_peers_add_verb():
    """SEC 1.11: there is no ``peers add`` — peers only come from pairing; the error says how to pair."""
    parsed = surfaces.parse("peers add laptop https://x --key abc")
    assert parsed.errors
    assert "pair" in parsed.errors[0]
    assert "add" not in surfaces.PEERS_SUBVERBS


def test_options_from_settings_and_flags():
    settings = surfaces.settings_from_config(
        {
            "plugins": {
                "entries": {
                    "ctx_handover": {
                        "settings": {"max_total_gb": 2, "wait_timeout_min": 5, "allow_roots": ["~/w"], "refine": "off"}
                    }
                }
            }
        }
    )
    parsed = surfaces.parse(
        "to server --dest ~/x --map /a=/b --include i --include-secret s --exclude e --no-workspace --model m "
        "--wait --continue go --yes --max-total-gb 3"
    )
    o = surfaces.options_from(parsed, settings)
    assert isinstance(o, Options)
    assert o.dest == "~/x" and o.maps == (("/a", "/b"),)
    assert o.hints.include == ("i",) and o.hints.include_secret == ("s",) and o.hints.exclude == ("e",)
    assert o.hints.no_workspace is True and o.hints.dest_overrides == (("/a", "/b"),)
    assert o.model == "m" and o.wait is True and o.yes is True and o.continue_prompt == "go"
    assert o.max_total_bytes == 3 * 1024**3
    assert o.wait_timeout_s == 300 and o.allow_roots == ("~/w",) and o.refine == "off"
    plain = surfaces.options_from(surfaces.parse("take"), surfaces.settings_from_config({}))
    assert plain.max_total_bytes == 10 * 1024**3 and plain.continue_prompt is None
    assert plain.confirm_above_bytes == 500 * 1024**2 and plain.staging_ttl_s == 86400
    assert plain.participant_idle_timeout_s == 900 and plain.stall_timeout_s == 1800
    exp = surfaces.options_from(surfaces.parse("export --keep --dir"), surfaces.settings_from_config({}))
    assert exp.keep_source is True and exp.as_dir is True
    imp = surfaces.options_from(surfaces.parse("import b --trust-unsigned --again"), surfaces.settings_from_config({}))
    assert imp.trust_unsigned is True and imp.again is True


def test_gate_from_settings():
    s = surfaces.settings_from_config(
        {
            "plugins": {
                "entries": {
                    "ctx_handover": {
                        "settings": {
                            "peer_gate": "basic_login",
                            "peer_username": "me",
                            "peer_extra_headers": {"CF-Access-Client-Id": "CF_ID"},
                        }
                    }
                }
            }
        }
    )
    gate = surfaces.gate_from_settings(s, {})
    assert gate.mode is GateMode.BASIC_LOGIN and gate.username == "me"
    assert gate.token_env == "CTX_HANDOVER_PEER_TOKEN" and gate.password_env == "CTX_HANDOVER_PEER_PASSWORD"
    assert gate.extra_headers == (("CF-Access-Client-Id", "CF_ID"),)
    assert surfaces.gate_from_settings(s, {"gate": GateMode.BEARER}).mode is GateMode.BEARER
    assert surfaces.gate_from_settings(surfaces.settings_from_config({}), {}).mode is GateMode.SESSION_TOKEN


def test_error_texts_table():
    """BX M8, DESIGN §14.1: every code maps to its remedy text (what to do, not just what failed)."""
    p = "server"
    cases = [
        (GateRejected("401"), ["dashboard of server rejected", "HERMES_DASHBOARD_SESSION_TOKEN on server"]),
        (ListenerMissing("404"), ["server has no ctx_handover routes", "restart `hermes serve` and the gateway"]),
        (HttpsRequired("x"), ["plain HTTP", "--allow-insecure", "HTTPS"]),
        (PairInvalid("x"), ["wrong or expired (10 min, single use)", "/handover pair again on server"]),
        (Unauthorized("x", skew_s=7), ["did not accept this device's key", "Pair again", "skew 7 s"]),
        (Forbidden("x", right="pull"), ["does not allow pull for this device", "Pair again with the needed rights"]),
        (SessionBusy("x"), ["A turn is running in this session", "--wait"]),
        (WaitTimeout("x", minutes=30), ["did not finish within 30 min", "Nothing was changed"]),
        (
            AlreadyHandingOver("x", job="01JCAAAAAAAAAAAAAAAAABCDEF"),
            ["already being handed over (job ABCDEF)", "/handover status"],
        ),
        (
            CapabilityMissing("x", capability="git.bundle.v1", plugin_version="0.1.0", hermes_version="0.22.6"),
            ["server cannot git.bundle.v1", "Update the plugin there"],
        ),
        (PayloadTooLarge("x"), ["limits request bodies below one block", "Raise its body limit"]),
        (
            InsufficientStorage("x", side="laptop", need_bytes=3 * 1024**3, free_bytes=1024**3, path="/home/u"),
            ["laptop needs 3.0 GiB free in /home/u, has 1.0 GiB", "Nothing was transferred"],
        ),
        (
            PolicyDenied("x", count=2, items=["/etc/x", "/root/y"]),
            ["2 items are not allowed on server", "/etc/x", "allow_roots", "--dest"],
        ),
        (RestartLimit("x"), ["kept changing", "3 restarts", "try again"]),
        (VerifyFailed("x"), ["did not match", "Nothing was activated", "source is unchanged"]),
        (InDoubt("x"), ["may already have the session", "/handover reclaim --force"]),
        (
            PeerUnreachable("x", reason="timeout", job="01JCAAAAAAAAAAAAAAAAABCDEF", release_at="14:05"),
            ["server is not reachable (timeout)", "suspended", "14:05", "/handover resume ABCDEF"],
        ),
        (
            ContinueRefused("x", reason="not_allowed"),
            ["does not allow remote continuation (allow_remote_continue)", "Open the session there and send a message"],
        ),
        (ContinueRefused("x", reason="approvals_off"), ["approvals are switched off", "Open the session"]),
    ]
    for err, needles in cases:
        text = surfaces.render_error(err, p)
        for needle in needles:
            assert needle in text, (err.code, needle, text)
    assert "basic" in surfaces.render_error(GateRejected("x", mode="basic_login"), p).lower()
    generic = surfaces.render_error(HandoverError("boom"), p)
    assert "boom" in generic


def _ack(**over):
    base = {
        "session_id": "20261109_141003_a1b2c3",
        "root_id": "20261109_141003_a1b2c3",
        "title": "My Work",
        "dests": (("w0", "/home/u/app", DestMode.FRESH),),
        "path_map": (PathMapEntry(source="/srv/app", target="/home/u/app", kind="workspace"),),
        "model": "qwen3",
        "offline_capable": True,
        "warnings": (),
        "committed_at": 1.0,
    }
    base.update(over)
    return CommitAck(**base)


def test_final_message_fields():
    """★ R14, BX P6, SK L8: the final message names the target session, its folder, the model, whether it works
    offline and the next step; a pull whose remote retire has not happened yet says the source is released on the
    next contact (and never claims "done").

    Red proof: mutant = ``render_final`` drops the offline clause (``offline = ""``) → ``AssertionError: assert
    'works offline: yes' in '"My Work" is here now (session …)…'``.
    """
    pull = surfaces.render_final(Mode.PULL, _ack(), "done", "server", None)
    for needle in (
        '"My Work" is here now',
        "20261109_141003_a1b2c3",
        "/home/u/app",
        "model qwen3",
        "works offline: yes",
        "Open it from the session list and send a message",
    ):
        assert needle in pull, needle
    online = surfaces.render_final(Mode.PULL, _ack(offline_capable=False, model="gpt-x"), "done", "server", None)
    assert "works offline: no" in online and "model gpt-x" in online
    early = surfaces.render_final(Mode.PULL, _ack(), "committed", "server", None)
    assert "server will be released on the next contact" in early
    assert '"My Work" is active here' in early

    push = surfaces.render_final(Mode.PUSH, _ack(model="gpt-x", offline_capable=False), "done", "server", None)
    for needle in (
        'Handed over to server: "My Work" continues there',
        "session 20261109_141003_a1b2c3",
        "/home/u/app",
        "model gpt-x",
        "read-only here",
        "/handover reclaim",
    ):
        assert needle in push, needle
    cont = surfaces.render_final(Mode.PUSH, _ack(), "done", "server", ContinueResult(started=True, pid=12))
    assert "The agent is continuing on server." in cont
    refused = surfaces.render_final(
        Mode.PUSH, _ack(), "done", "server", None, continue_error=ContinueRefused("x", reason="not_allowed")
    )
    assert "allow_remote_continue" in refused and "Handed over to server" in refused

    exported = surfaces.render_final(
        Mode.EXPORT,
        None,
        "done",
        None,
        None,
        title="My Work",
        bundle_path="/home/u/hermes-handover/my-work.ctxh.tar",
        bundle_bytes=3 * 1024**2,
        remote_gateway=True,
    )
    for needle in ("/home/u/hermes-handover/my-work.ctxh.tar", "3.0 MiB", "read-only here", "file view"):
        assert needle in exported, needle
    kept = surfaces.render_final(Mode.EXPORT, None, "done", None, None, title="T", bundle_path="/b", keep=True)
    assert "stays active here" in kept


def test_final_message_shows_note_and_origin():
    """SEC 4.6: take/import show the note text, the origin, excluded secrets, withheld stash entries, conflicts and
    instruction files; the user is told the note is not shown in the transcript view."""
    text = surfaces.render_final(
        Mode.IMPORT,
        _ack(),
        "done",
        None,
        None,
        note="<ctx-handover-note>Moved from: server</ctx-handover-note>",
        origin="unverified bundle",
        excluded_secrets=["/srv/app/.env.local"],
        withheld_stash=["stash@{1}: .env"],
        conflicts=["/home/u/app → /home/u/app-handover-20261109 (local changes)"],
        instruction_files=["AGENTS.md (changed)"],
    )
    for needle in (
        "Moved from: server",
        "Origin: unverified bundle",
        "/srv/app/.env.local",
        "stash@{1}",
        "app-handover-20261109",
        "AGENTS.md (changed)",
        "not shown in the transcript",
    ):
        assert needle in text, needle
    verified = surfaces.render_final(Mode.PULL, _ack(), "done", "server", None, origin="verified peer server", note="n")
    assert "Origin: verified peer server" in verified


def test_render_peers_status_plan_offer():
    peers = [
        Peer(
            name="server",
            peer_id="01P1",
            direction="dial",
            url="https://s.example",
            pubkey="A" * 43,
            rights=frozenset({Right.PULL, Right.PUSH}),
            last_seen_at=1_762_696_983.0,
            last_ip="203.0.113.9",
        ),
        Peer(
            name="old",
            peer_id="01P2",
            direction="inbound",
            url=None,
            pubkey="B" * 43,
            rights=frozenset({Right.PULL}),
            revoked_at=5.0,
        ),
    ]
    text = surfaces.render_peers(peers)
    assert "server" in text and "dial" in text and "https://s.example" in text and "pull,push" in text
    assert "as reported by the proxy" in text and "revoked" in text
    assert "No paired devices" in surfaces.render_peers([])

    status = surfaces.render_status(
        {
            "jobs": [
                {
                    "job_id": "01JCAAAAAAAAAAAAAAAAABCDEF",
                    "mode": "pull",
                    "state": "packing",
                    "peer": "server",
                    "progress": {"step": "packing workspace", "bytes_done": 10, "bytes_total": 20},
                    "warnings": ["w1"],
                    "last_error": None,
                    "next_action": "wait",
                }
            ],
            "session": {"source": {"state": "retired", "peer": "server", "job_id": "01JCX"}},
            "ctx": {"pending": 2, "server": "ctx"},
            "disk": {"blocks_bytes": 1024**2, "staging_bytes": 0},
            "pulled_by": [{"peer": "laptop", "at": 1.0, "items": ["/srv/app"]}],
        }
    )
    for needle in ("ABCDEF", "pull", "packing workspace", "w1", "retired", "2 ctx records waiting", "pulled by laptop"):
        assert needle in status, needle
    assert "No handovers yet" in surfaces.render_status({"jobs": []})

    plan = surfaces.render_plan(
        {
            "items_count": 3,
            "total_bytes": 2 * 1024**3,
            "proposed": 1,
            "conflicts": 0,
            "model": "qwen3",
            "offline": False,
            "excluded_secrets": [".env"],
            "items": [{"item_id": "w0", "source_path": "/srv/app", "include": "yes", "size": 1024, "dest": "/x"}],
        }
    )
    assert "3 items" in plan and "2.0 GiB" in plan and "offline: no" in plan and "/srv/app" in plan

    offer = surfaces.render_offer({"offer_id": "01OFFER", "expires_at": 7200.0}, "laptop", "server", now=0.0)
    assert "Offer created for laptop (valid 2 h)" in offer and "/handover take server" in offer


def test_confirm_question_and_choices():
    q = surfaces.confirm_question(
        {
            "items_count": 4,
            "total_bytes": 600 * 1024**2,
            "proposed": 2,
            "conflicts": 1,
            "model": "qwen3",
            "offline": True,
        },
        "server",
    )
    assert q.startswith("Hand over to server: 4 items, 600.0 MiB, 2 proposed, 1 conflicts, model qwen3 (offline: yes).")
    assert q.rstrip().endswith("Continue?")
    assert [c["value"] for c in surfaces.CONFIRM_CHOICES] == ["yes", "no_proposed", "show_plan", "cancel"]


def test_format_progress():
    from ctx_handover.engine.model import Progress
    from ctx_handover.engine.progress import format_progress

    line = format_progress(
        Progress(step="packing workspace", bytes_done=312 * 1024**2, bytes_total=1024 * 1024**2),
        "laptop",
        "out",
        eta_s=120,
    )
    assert line == "Handover → laptop · packing workspace · 312/1024 MiB (30 %) · ~2 min left"
    inbound = format_progress(Progress(step="importing", items_done=2, items_total=7), "server", "in")
    assert inbound == "Handover ← server · importing · 2/7"
