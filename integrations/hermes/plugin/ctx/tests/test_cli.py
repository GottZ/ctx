"""``hermes ctx`` CLI: argparse tree, provenance tags, sensitivity default, § parsing, exit codes."""

from __future__ import annotations

import argparse
import json
from typing import ClassVar

import pytest

from ctx_plugin import cli
from ctx_plugin.client import CtxApiError
from ctx_plugin.config import DEFAULT_PREFETCH_TYPES_EXCLUDE


class CliFake:
    """Stands in for CtxClient inside cli.py; one shared call log per test."""

    log: ClassVar[list] = []

    def __init__(self, base_url, api_key, timeout=10.0, synthesis_timeout=90.0):
        CliFake.log.append(("init", {"base_url": base_url, "timeout": timeout}))
        self.store_result = None

    def whoami(self):
        return {"success": True, "home_scope": "shared", "label": "cli-key", "role": "writer"}

    def store(self, category, title, content, sensitivity=None, tags=None, **extra):
        CliFake.log.append(
            (
                "store",
                {
                    "category": category,
                    "title": title,
                    "content": content,
                    "sensitivity": sensitivity,
                    "tags": tags,
                    **extra,
                },
            )
        )
        if "fail" in title:
            raise CtxApiError("HTTP 403 [reserved_category]: nope", status=403, code="reserved_category")
        return {"success": True, "block": {"id": f"blk-{len(CliFake.log)}", "created_at": "t", "updated_at": "t"}}

    def query(self, text, **kwargs):
        CliFake.log.append(("query", {"text": text, **kwargs}))
        return {
            "success": True,
            "answer": "42",
            "sources": [{"id": "1", "title": "T", "category": "c", "type": "insight", "untrusted": True}],
        }

    def search(self, text, **kwargs):
        CliFake.log.append(("search", {"text": text, **kwargs}))
        return {"success": True, "count": 0, "results": []}

    def get_block(self, block_id, **kwargs):
        CliFake.log.append(("get", {"block_id": block_id}))
        return {"success": True, "block": {"id": block_id}}


@pytest.fixture
def run(monkeypatch, tmp_path):
    home = tmp_path / "profiles" / "ops"
    (home / "memories").mkdir(parents=True)
    (home / "ctx.json").write_text(json.dumps({"sensitivity_default": "internal"}), encoding="utf-8")
    monkeypatch.setenv("CTX_KEY", "test-key-not-real")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid/")
    monkeypatch.setattr(cli, "CtxClient", CliFake)
    monkeypatch.setattr(cli, "_hermes_home", lambda: home)
    CliFake.log = []
    parser = argparse.ArgumentParser(prog="hermes ctx")
    cli.register_cli(parser)

    def _run(*argv: str) -> int:
        args = parser.parse_args(list(argv))
        try:
            args.func(args)
        except SystemExit as exc:
            return int(exc.code or 0)
        return 0

    _run.home = home
    return _run


def _calls(kind):
    return [kw for name, kw in CliFake.log if name == kind]


def test_sync_splits_on_separator_lines_only_and_tags_writes(run, capsys):
    (run.home / "memories" / "MEMORY.md").write_text(
        "Rechnungen gemäß § 22 UStG werden quartalsweise an die Steuerberatung gemeldet.\n§\n"
        "Backups laufen nächtlich um 02:00 auf das NAS.\n",
        encoding="utf-8",
    )
    (run.home / "memories" / "USER.md").write_text("Bevorzugt kurze Antworten auf Deutsch.", encoding="utf-8")
    assert run("sync") == 0
    stores = _calls("store")
    assert [s["category"] for s in stores] == ["agent-briefing", "agent-briefing", "reference"]
    assert stores[0]["content"].startswith("Rechnungen gemäß § 22 UStG")  # inline § is not a separator
    assert stores[0]["title"].startswith("memory: ") and stores[2]["title"].startswith("user: ")
    for s in stores:
        assert s["tags"] == ["source:hermes", "profile:ops", "hook:cli"], "upsert without tags would wipe them"
        assert s["sensitivity"] == "internal"
    assert "memory: 2/2 entries mirrored (2 created)" in capsys.readouterr().out
    assert _calls("init")[0]["base_url"] == "http://ctx.invalid"


def test_memory_file_parsing_edge_cases(tmp_path):
    path = tmp_path / "MEMORY.md"
    path.write_text("eins\r\n§\r\n§\r\nzwei mit § 5\r\n  §  \r\n\r\ndrei\r\n§\r\n", encoding="utf-8")
    assert cli._parse_memory_file(path) == ["eins", "zwei mit § 5", "drei"]
    assert cli._parse_memory_file(tmp_path / "missing.md") == []


def test_sync_reports_failures_with_exit_code(run, capsys):
    (run.home / "memories" / "MEMORY.md").write_text("ok entry\n§\nfail entry", encoding="utf-8")
    assert run("sync") == 1
    out = capsys.readouterr().out
    assert "memory: 1/2 entries mirrored" in out and "reserved_category" in out


def test_store_uses_sensitivity_default_tags_and_prints_block_id(run, capsys):
    assert run("store", "infrastructure", "INFRA-GIT", "Host 192.0.2.10", "--tag", "git", "--tag", "hook:spoof") == 0
    (store,) = _calls("store")
    assert store["sensitivity"] == "internal"
    assert store["tags"] == ["git", "source:hermes", "profile:ops", "hook:cli"]
    assert "✓ created: [infrastructure] INFRA-GIT (id: blk-" in capsys.readouterr().out
    run("store", "c", "t", "x", "--sensitivity", "public")
    assert _calls("store")[-1]["sensitivity"] == "public"


def test_store_rejection_exits_nonzero(run, capsys):
    assert run("store", "c", "fail", "x") == 1
    assert "reserved_category" in capsys.readouterr().out


def test_search_and_query_honour_types_exclude(run, capsys):
    run("search", "proxy", "--tag", "infra")
    run("search", "proxy", "--all-types")
    first, second = _calls("search")
    assert first["types_exclude"] == list(DEFAULT_PREFETCH_TYPES_EXCLUDE) and first["tags"] == ["infra"]
    assert second["types_exclude"] is None
    assert run("query", "was ist x", "--synthesize") == 0
    (query,) = _calls("query")
    assert query["synthesize"] is True and query["types_exclude"] == list(DEFAULT_PREFETCH_TYPES_EXCLUDE)
    out = capsys.readouterr().out
    assert "42" in out and "[untrusted" in out and "<insight>" in out


def test_get_goes_through_client_get_block(run, capsys):
    assert run("get", "019f6000-0000-7000-8000-000000000001") == 0
    assert _calls("get") == [{"block_id": "019f6000-0000-7000-8000-000000000001"}]


def test_status_shows_profile_and_settings(run, capsys):
    assert run("status") == 0
    out = capsys.readouterr().out
    assert "profile:  ops" in out and "✓ connected · scope: shared" in out and "sensitivity_default: internal" in out


def test_unconfigured_and_usage_exit_codes(run, monkeypatch, capsys):
    assert run() == 2
    assert "Usage: hermes ctx <status|sync|query|store|search|get>" in capsys.readouterr().out
    monkeypatch.delenv("CTX_KEY")
    assert run("search", "x") == 1
    assert "CTX not configured" in capsys.readouterr().out
    assert run("status") == 1
    assert "key:      NOT set" in capsys.readouterr().out
    assert _calls("search") == []
