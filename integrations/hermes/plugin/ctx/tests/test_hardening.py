"""Hardening tests (2026-10-20): provenance tags, fact-loop tolerance, log levels."""

from __future__ import annotations

import json
import logging

import pytest
from conftest import join_writes

from ctx_plugin.client import store_action, store_block_id
from ctx_plugin.config import derive_profile_name, merge_tags

PROVIDER_LOGGER = "ctx_plugin.provider"

FACTS_MSGS = [
    {
        "role": "user",
        "content": f"Der Server srv{i} nutzt Port {8000 + i} fuer den Reverse-Proxy und das Monitoring-Backend.",
    }
    for i in range(3)
]


# -- Profile name / tag derivation -----------------------------------------------


@pytest.mark.parametrize(
    "home,expected",
    [
        ("/home/thel/.hermes/profiles/default", "default"),
        ("/home/thel/.hermes/profiles/sysadmin/", "sysadmin"),
        ("/home/thel/.hermes", "default"),
        ("", "default"),
        (None, "default"),
        ("/srv/hermes-home", "default"),
    ],
)
def test_derive_profile_name(home, expected):
    assert derive_profile_name(home) == expected


def test_merge_tags_keeps_agent_tags_and_appends_origin():
    tags = merge_tags(["infra", "dns", "infra", " ", "profile:spoof", "hook:x"], "default", "tool")
    assert tags == ["infra", "dns", "source:hermes", "profile:default", "hook:tool"]


def test_merge_tags_accepts_comma_string_and_none():
    assert merge_tags("a, b", "p", "tool") == ["a", "b", "source:hermes", "profile:p", "hook:tool"]
    assert merge_tags(None, "p", "memory_write") == ["source:hermes", "profile:p", "hook:memory_write"]


def test_store_action_classification():
    assert store_action({"action": "noop", "success": True}) == "noop"
    assert store_action({"success": True, "block": {"created_at": "t1", "updated_at": "t1"}}) == "created"
    assert store_action({"success": True, "block": {"created_at": "t1", "updated_at": "t2"}}) == "updated"
    assert store_block_id({"action": "noop", "existing_id": "x"}) == "x"
    assert store_block_id({"block": {"id": "y"}}) == "y"


def test_auto_hooks_send_origin_tags(make_provider):
    p, client = make_provider("default", config={"facts_from_user": False})  # FACTS_MSGS = user turns
    p.on_memory_write("add", "memory", "Hermes nutzt CTX als Memory-Provider fuer alle Profile.")
    join_writes(p)
    msgs = [{"role": "user", "content": "x" * 10}, {"role": "assistant", "content": "y" * 10}] * 2
    p.on_session_end(msgs)
    join_writes(p)
    p.on_pre_compress(FACTS_MSGS[:1])
    join_writes(p)
    hooks = [c["tags"] for c in client.calls]
    assert hooks[0] == ["source:hermes", "profile:default", "hook:memory_write"]
    assert hooks[1] == ["source:hermes", "profile:default", "hook:session_end"]
    assert hooks[2] == ["source:hermes", "profile:default", "hook:pre_compress"]


def test_base_home_maps_to_default_profile(make_provider):
    p, _client = make_provider(None)
    assert p._profile == "default"


def test_ctx_store_tool_merges_agent_tags(make_provider):
    p, client = make_provider("sysadmin")
    out = json.loads(
        p.handle_tool_call(
            "ctx_store",
            {
                "category": "learnings",
                "title": "t",
                "content": "c",
                "tags": ["dns", "source:evil"],
            },
        )
    )
    assert out["saved"] is True and out["id"] == "blk-0"
    assert client.calls[0]["tags"] == ["dns", "source:hermes", "profile:sysadmin", "hook:tool"]


# -- Fact loop tolerance -----------------------------------------------------------


def test_fact_loop_continues_after_failure(make_provider, caplog):
    p, client = make_provider(config={"facts_from_user": False})
    client.fail_on = {0}  # first fact raises
    client.reject_on = {1}  # second fact rejected by server (success: false)
    caplog.set_level(logging.DEBUG, logger=PROVIDER_LOGGER)
    summary_text = p.on_pre_compress(FACTS_MSGS)
    join_writes(p)
    assert "Facts extracted" in summary_text
    assert len(client.calls) == 3, "loop must not abort on the first error"
    summary = [r for r in caplog.records if "pre_compress facts done" in r.getMessage()]
    assert len(summary) == 1
    assert summary[0].levelno == logging.WARNING
    assert "stored=1 failed=2 of 3" in summary[0].getMessage()


def test_fact_loop_all_ok_summary_is_info(make_provider, caplog):
    p, _client = make_provider(config={"facts_from_user": False})
    caplog.set_level(logging.DEBUG, logger=PROVIDER_LOGGER)
    p.on_pre_compress(FACTS_MSGS)
    join_writes(p)
    summary = [r for r in caplog.records if "pre_compress facts done" in r.getMessage()]
    assert summary and summary[0].levelno == logging.INFO
    assert "stored=3 failed=0 of 3" in summary[0].getMessage()


# -- Logging levels / no content in logs -------------------------------------------

SECRET_CONTENT = "Geheimer Inhalt: Passwort hunter2 steht im Tresor, nicht loggen bitte, niemals."


def test_success_logged_at_info_with_hook_profile_action(make_provider, caplog):
    p, _client = make_provider("default")
    caplog.set_level(logging.DEBUG, logger=PROVIDER_LOGGER)
    p.on_memory_write("add", "user", SECRET_CONTENT)
    join_writes(p)
    recs = [r for r in caplog.records if r.name == PROVIDER_LOGGER and "store" in r.getMessage()]
    assert recs and recs[0].levelno == logging.INFO
    msg = recs[0].getMessage()
    assert "memory_write" in msg and "profile=default" in msg and "created" in msg and "id=blk-0" in msg
    # only the (shortened, 60-char derived) title may appear, never the full content
    assert SECRET_CONTENT not in caplog.text


def test_failure_logged_at_warning(make_provider, caplog):
    p, client = make_provider()
    client.fail_on = {0}
    caplog.set_level(logging.DEBUG, logger=PROVIDER_LOGGER)
    p.on_memory_write("add", "memory", SECRET_CONTENT)
    join_writes(p)
    recs = [r for r in caplog.records if r.name == PROVIDER_LOGGER and "store failed" in r.getMessage()]
    assert recs and recs[0].levelno == logging.WARNING
    assert "memory_write" in recs[0].getMessage()
    assert SECRET_CONTENT not in caplog.text


def test_session_end_rejection_logged_at_warning(make_provider, caplog):
    p, client = make_provider()
    client.reject_on = {0}
    caplog.set_level(logging.DEBUG, logger=PROVIDER_LOGGER)
    msgs = [{"role": "user", "content": "frage " * 5}, {"role": "assistant", "content": "antwort " * 5}] * 2
    p.on_session_end(msgs)
    join_writes(p)
    recs = [r for r in caplog.records if "session_end store rejected" in r.getMessage()]
    assert recs and recs[0].levelno == logging.WARNING
