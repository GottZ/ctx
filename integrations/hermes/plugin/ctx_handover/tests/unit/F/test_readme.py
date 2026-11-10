"""Cluster F — README: limits first, lock guarantee, disclosure (DESIGN §1.2, §1.4, §16; SEC 7.5; R8, R17)."""

from __future__ import annotations

import ast
import re

from ctx_handover.engine import commands, surfaces

from .test_surfaces import PLUGIN_ROOT

README = (PLUGIN_ROOT / "README.md").read_text(encoding="utf-8")
FLAT = re.sub(r"\s+", " ", README)

SEC_7_5 = [
    # 1 — the stored gate credential gives full access to the peer's dashboard (SEC 1.2, R17)
    "the dashboard token or Basic login you store for a peer opens that peer's whole dashboard",
    # 2 — pull/push scope (SEC 2.2)
    "`pull` lets the peer take sessions of this profile with the files of their own plan and of `export_roots`",
    "`push` lets it create sessions and workspace copies under `allow_roots`",
    "never in Hermes, credential, shell-startup, autostart, git or ssh configuration locations",
    # 3 — unredacted transcript and side files, unencrypted bundles
    # (review L22: every session file kind, terminal output and attached images included — test_rv_f_readme)
    "transcripts, spillover, hook-output and terminal-output files and attached images go to the peer as they are",
    "Bundles contain the raw transcript and workspace unencrypted",
    # 4 — committed history incl. tracked secret files
    "committed history travels with a repository, including tracked secret files",
    # 5 — workspace .env only with --include-secret; the sniff reads 8 KiB
    "workspace `.env` files only with `--include-secret` per path on the source",
    "The content sniff reads the first 8 KiB of each candidate file",
    # 6 — unattended turn with allow_remote_continue / allow_agent_start
    "With `allow_remote_continue` or `allow_agent_start`, a turn runs without a person present",
    # 7 — block cache retention
    "The block cache keeps copies up to 3 days / 5 GiB",
    # 8 — host isolation buffers before the plugin's limit
    "Under `plugins.isolation: host` the core buffers request bodies before the plugin's size limit applies",
]

DESIGN_16_HEADINGS = [
    "**Network:**",
    "**Inbound:**",
    "**Reads:**",
    "**Writes:**",
    "**Commands:**",
    "**Background work:**",
    "**Stored secrets:**",
    "**Model use",
    "**ctx (optional):**",
    "**Core modules used:**",
    "**What the peer credential means:**",
    "**Rights:**",
    "**Unredacted:**",
    "**Not moved:**",
]


def test_readme_disclosure_contains_required_lines():
    """SEC 7.5 (all eight additions), DESIGN §16 sections, R17 and R8 are in the README."""
    for line in SEC_7_5:
        assert line in FLAT, line
    for heading in DESIGN_16_HEADINGS:
        assert heading in FLAT, heading
    assert "Works without ctx." in FLAT
    assert "Not the core `/handoff`" in FLAT
    assert (
        "Gateways that Hermes Desktop reaches only through an SSH tunnel, an OAuth/Nous sign-in or Hermes Cloud "
        "have no credential a plugin can hold. For them this version offers the bundle path only"
    ) in FLAT
    assert "it never carries a dashboard credential" in FLAT


def test_readme_limits_first_and_lock_guarantee():
    """DESIGN §1.2: the limits come before any use — the first section ("Before you start") names the main ones
    (bundle path only for SSH tunnel / OAuth / Hermes Cloud, the laptop dials, what is not moved, no merge) and points
    to the full list, which opens the reference; §1.4: what the lock guarantees and what not, in plain words."""
    sections = re.findall(r"^## (.+)$", README, flags=re.MULTILINE)
    assert sections[0] == "Before you start", sections
    first = README.split("## Before you start", 1)[1].split("\n## ", 1)[0]
    for needle in (
        "only through an SSH tunnel, a Nous/OAuth sign-in or Hermes Cloud",
        "only the handover file works",
        "The laptop always connects to the server, never the other way round.",
        "Running programs, subagents and scheduled (cron) jobs do not move.",
        "they are never merged",
        "[Limits](#limits)",
    ):
        assert needle in re.sub(r"\s+", " ", first), needle
    reference = re.findall(r"^### (.+)$", README.split("\n## Reference\n", 1)[1], flags=re.MULTILINE)
    assert reference[:2] == ["Words used below", "Limits"], reference
    for needle in (
        "(1) ended with reason `handed_over`, (2) archived, (3) hidden and (4) renamed",
        "hold even when the plugin is disabled, not loaded (safe mode) or broken",
        "the two copies are then independent (no merge)",
        "running processes, subagents/delegations and cron jobs",
    ):
        assert needle in FLAT, needle


def test_readme_matches_code():
    """Statements the README makes about the surfaces equal the code: verbs, local-only verbs, secret env names,
    settings keys, exit codes, core modules imported by hostcompat."""
    for verb in surfaces.VERBS:
        assert f"{verb}" in README, verb
    listed = re.search(r"Administrative and data-moving commands \(([^)]*)\)", FLAT).group(1)
    assert {v.strip(" `") for v in listed.split(",")} == set(commands.LOCAL_ONLY_VERBS)
    for env in surfaces.SECRET_ENVS.values():
        assert env in README, env
    table = README.split("## Settings", 1)[1].split("## Disclosure", 1)[0]
    keys = set(re.findall(r"`([a-z_]+)`", table))
    unknown = {k for k in keys if "_" in k or k in {"inbound", "refine"}} - set(surfaces.SETTINGS_SCHEMA)
    assert unknown <= {"session_token", "side_question", "config_schema"}, unknown  # values, not keys
    assert "0 ok, 1 failed, 2 usage, 3 busy/conflict, 4 auth/gate, 5 in-doubt/suspended" in FLAT
    tree = ast.parse((PLUGIN_ROOT / "engine" / "hostcompat.py").read_text(encoding="utf-8"))
    imported = {n.module for n in ast.walk(tree) if isinstance(n, ast.ImportFrom) and n.module and n.level == 0}
    imported |= {a.name for n in ast.walk(tree) if isinstance(n, ast.Import) for a in n.names}
    hermes = {
        m
        for m in imported
        if m.split(".")[0]
        in {"hermes_state", "hermes_constants", "hermes_cli", "agent", "plugins", "gateway", "providers"}
    }
    core = FLAT.split("**Core modules used:**", 1)[1].split("No runtime overrides", 1)[0]
    for module in hermes:
        assert f"`{module}`" in core, module
