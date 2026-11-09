"""K0: the frozen surface — every INTERFACES §3 name exists with its parameters (in order) and a docstring.

Additive rule: K0 may append keyword parameters with defaults after the specified ones; it never renames,
reorders or removes one. This test is the tripwire for that rule. Layer 2 (contract K-1…K-5) added names and
parameters; they are frozen here too (``ADDED_PARAMETERS``, ``ADDED_FIELDS``), dataclass field lists are frozen
in full, and every parameter or field beyond a frozen list must carry a default — a new required parameter or
field is not additive and fails here.
"""

from __future__ import annotations

import dataclasses
import inspect
import re
from collections.abc import Mapping

import pytest

from ctx_handover.engine import contracts, errors, hostcompat, ids, ignore, jsonutil, model, paths, policy, settings

FUNCTIONS = {
    paths: {
        "rules_for": ["os_family", "case_insensitive", "long_paths"],
        "check_rel": ["rel", "rules"],
        "safe_join": ["root", "rel", "rules"],
        "under_any": ["path", "roots"],
        "expand_root": ["spec", "home"],
        "home_relative": ["path", "home"],
        "fold_key": ["rel", "rules"],
        "collisions": ["rels", "rules"],
        "strip_hfs_ignorable": ["rel"],
        "reject_windows_device_prefix": ["path"],
        "route_path": ["route"],
        "msys_to_windows": ["path"],
        "strip_url_userinfo": ["url"],
        "default_workspace_root": ["home", "hermes_home"],
        "expand_handover_root": ["spec", "home", "hermes_home"],
    },
    policy: {
        "is_denied_name": ["rel_or_path", "case_insensitive"],
        "sniff_secret": ["path"],
        "suspected_secret": ["path", "redact"],
        "is_hermes_protected": ["path", "cfg", "session_file_kind"],
        "check_source_file": ["path", "cfg", "explicit"],
        "check_target_dest": ["path", "cfg", "recorded", "local_choice"],
        "check_tree_entry": ["entry"],
        "exclude_pathspecs": ["paths"],
        "check_extra": ["path", "cfg", "explicit", "redact"],
        "sniff_bytes": ["head"],
        "reserved_workspace_roots": ["cfg"],
    },
    jsonutil: {
        "canonical_bytes": ["obj"],
        "sha256_hex": ["data"],
        "doc_id": ["obj"],
        "loads_bounded": ["data", "max_bytes", "max_depth"],
        "to_wire": ["obj"],
        "from_wire": ["cls", "data"],
        "object_manifest_doc": ["sha256", "size", "block_size", "blocks", "media"],
        "object_manifest": ["data", "media", "block_size"],
        "object_id": ["manifest_doc"],
        "check_object_manifest": ["doc", "object_id"],
    },
    ids: {
        "ulid": [],
        "crockford": ["nbytes"],
        "short": ["job_id"],
        "mint_session_id": ["now"],
        "is_ulid": ["value"],
    },
    model: {"added_field": ["default"]},
    hostcompat: {
        "features": ["ctx"],
        "session_db": [],
        "state_db_path": [],
        "hermes_paths": [],
        "host_facts": [],
        "plugin_dir": [],
        "load_config": [],
        "read_secret": ["name"],
        "env_writable": ["name"],
        "save_env": ["name", "value"],
        "provider_usable": ["name", "config"],
        "redact": ["text"],
        "plugin_llm": [],
        "run_in_context": ["fn", "a", "kw"],
        "hermes_root": [],
        "read_env_fresh": ["name"],
        "approvals_off": ["config"],
        "grant_mcp_server": ["server"],
    },
    settings: {
        "raw_settings": ["config"],
        "normalize": ["raw"],
        "from_config": ["config"],
        "problems": ["raw"],
        "coerce": ["key", "value"],
        "flag": ["value", "default"],
        "positive_int": ["value", "default"],
    },
}

ADDED_PARAMETERS = {
    (policy, "check_source_file"): {"item_root": None, "cache": None, "follow_link": None},
    (policy, "check_extra"): {"cache": None, "follow_link": None},
    (paths, "check_rel"): {"allow_control": False},
    (paths, "default_workspace_root"): {"hermes_root": None, "case_insensitive": False},
    (paths, "expand_handover_root"): {"hermes_root": None, "case_insensitive": False},
}
"""Keyword-only parameters added after a frozen prefix, with their defaults (contract K-4)."""

METHODS = {
    (ignore.IgnoreMatcher, "__init__"): ["self", "patterns"],
    (ignore.IgnoreMatcher, "for_tree"): ["root", "extra"],
    (ignore.IgnoreMatcher, "ignored"): ["self", "rel", "is_dir"],
    (policy.PolicyConfig, "from_options"): [
        "o",
        "home",
        "hermes_home",
        "hermes_root",
        "plugin_dir",
        "case_insensitive",
        "target_os",
        "include_secret",
    ],
    (errors.HandoverError, "__init__"): ["self", "message", "details", "retryable"],
    (errors.HandoverError, "to_wire"): ["self"],
    (errors.HandoverError, "from_wire"): ["status", "body"],
    (contracts.GitProbe, "is_partial"): ["self", "path"],
}

DATACLASS_FIELDS = {
    paths.PlatformRules: ["windows", "case_insensitive", "long_paths"],
    policy.Verdict: ["allowed", "reason"],
    policy.PolicyConfig: [
        "max_file_bytes",
        "max_extra_dir_bytes",
        "max_hermes_file_bytes",
        "max_files",
        "max_untracked_files",
        "max_total_bytes",
        "allow_roots",
        "export_roots",
        "home",
        "hermes_home",
        "hermes_root",
        "plugin_dir",
        "include_secret",
        "case_insensitive",
        "target_os",
        "document_cache",
    ],
    hostcompat.HostFeatures: [
        "side_question",
        "ask_choice",
        "post_message",
        "host_isolated",
        "session_db_ok",
        "plugin_llm",
        "missing",
    ],
    contracts.JobContext: ["job_id", "mode", "peer", "options", "epoch", "reporter", "cancelled", "created_at"],
    model.Options: [
        "dest",
        "maps",
        "hints",
        "model",
        "force",
        "dry_run",
        "yes",
        "wait",
        "wait_timeout_s",
        "continue_prompt",
        "keep_source",
        "trust_unsigned",
        "again",
        "as_dir",
        "max_total_bytes",
        "max_file_bytes",
        "max_extra_dir_bytes",
        "max_hermes_file_bytes",
        "max_files",
        "max_untracked_files",
        "workspace_root",
        "allow_roots",
        "export_roots",
        "max_session_bytes",
        "refine",
        "refine_timeout_s",
        "refine_history_chars",
        "refine_side_question_max_tokens",
        "refine_auto_include",
        "extra_paths",
        "include_hermes_files",
        "model_mapping",
        "lease_ttl_s",
        "participant_idle_timeout_s",
        "stall_timeout_s",
        "staging_ttl_s",
        "confirm_above_bytes",
        "extra_ignore",
        "tool_probe",
    ],
    model.Hello: [
        "protocol",
        "majors",
        "minor",
        "instance_id",
        "listener_pubkey",
        "plugin_version",
        "hermes_version",
        "name",
        "capabilities",
        "limits",
        "pairing_open",
        "peer",
        "server_time",
    ],
    model.EnvFacts: [
        "instance_name",
        "os_family",
        "os_name",
        "os_version",
        "arch",
        "hostname",
        "user",
        "home",
        "shell",
        "host",
        "tools",
        "tools_seen",
        "workspace_root",
        "allow_roots",
        "disk_free",
        "case_insensitive",
        "offline_capable",
        "locale",
        "timezone",
        "git_version",
        "git_lfs",
        "capabilities",
    ],
    model.PlanItem: [
        "item_id",
        "kind",
        "source_path",
        "role",
        "origin",
        "include",
        "reasons",
        "size_estimate",
        "git",
        "suggested_dest",
        "rel_cwd",
    ],
    model.ResourcePlan: [
        "job_id",
        "revision",
        "items",
        "not_transferred",
        "processes",
        "delegations",
        "cron",
        "refine",
        "tools_needed",
        "env_vars_needed",
        "notes_for_target",
        "approx_tokens",
    ],
    model.FinalPlan: ["plan_id", "plan", "decisions", "need_bytes", "free_bytes", "model_change"],
    model.ObjectManifest: ["sha256", "size", "block_size", "blocks", "media", "object_id"],
    model.TargetDecision: [
        "item_id",
        "dest",
        "mode",
        "reason",
        "conflict",
        "git_have",
        "git_seed",
        "need_bytes",
        "warnings",
    ],
    model.CommitAck: [
        "session_id",
        "root_id",
        "title",
        "dests",
        "path_map",
        "model",
        "offline_capable",
        "warnings",
        "committed_at",
    ],
}
"""Full field lists of commit ``c32a862d`` (before contract K-3); later fields only with a default."""

ADDED_FIELDS = {
    model.PlanItem: {"hermes_kind": None, "instruction_files": ()},
    model.ResourcePlan: {"warnings": ()},
    model.EnvFacts: {"python_version": "", "workspace_root_ok": None, "workspace_root_problem": ""},
}
"""Fields added by contract K-3, with their defaults; declared with ``model.added_field``."""

CONSTANTS = {
    policy: ["SOURCE_DENY_NAMES", "SOURCE_DENY_DIRS", "TARGET_DENY", "HERMES_CREDENTIAL_PATHS"],
    ignore: ["BUILTIN_IGNORES"],
}


def _params(fn) -> list[str]:
    return list(inspect.signature(fn).parameters)


@pytest.mark.parametrize(
    ("module", "name"),
    [(m, n) for m, names in FUNCTIONS.items() for n in names],
    ids=lambda x: getattr(x, "__name__", x),
)
def test_function_signature_prefix_and_docstring(module, name):
    fn = getattr(module, name)
    expected = FUNCTIONS[module][name]
    assert _params(fn)[: len(expected)] == expected
    assert inspect.getdoc(fn)


@pytest.mark.parametrize(("owner", "name"), list(METHODS), ids=lambda x: getattr(x, "__name__", x))
def test_method_signature_prefix_and_docstring(owner, name):
    fn = getattr(owner, name)
    expected = METHODS[(owner, name)]
    assert _params(fn)[: len(expected)] == expected
    assert inspect.getdoc(fn)


@pytest.mark.parametrize("cls", list(DATACLASS_FIELDS), ids=lambda c: c.__name__)
def test_dataclass_field_prefix(cls):
    names = [f.name for f in dataclasses.fields(cls)]
    expected = DATACLASS_FIELDS[cls]
    assert names[: len(expected)] == expected
    assert cls.__doc__ and dataclasses.fields(cls)


def test_constants_present():
    for module, names in CONSTANTS.items():
        for name in names:
            value = getattr(module, name)
            assert isinstance(value, tuple) and value and all(isinstance(v, str) for v in value)
    assert isinstance(ids.ULID_RE, re.Pattern) and ids.ULID_RE.match(ids.ulid())
    assert jsonutil.OBJECT_BLOCK_SIZE == 524_288 and jsonutil.OBJECT_MAX_BYTES == 30 * 1024**3
    assert isinstance(model.WIRE_OMIT_DEFAULT, str) and model.WIRE_OMIT_DEFAULT
    assert hostcompat.PLUGIN_ID == "ctx_handover" and isinstance(hostcompat.MCP_SERVER_RE, re.Pattern)
    assert settings.PLUGIN_ID == hostcompat.PLUGIN_ID
    for name in ("SCHEMA", "DEFAULTS", "SECRET_ENVS", "BOOL_WORDS"):
        assert isinstance(getattr(settings, name), Mapping) and getattr(settings, name), name
    assert isinstance(settings.POSITIVE_KEYS, frozenset)
    assert settings.POSITIVE_KEYS.issubset(settings.DEFAULTS)


def _extra_parameters_without_default(fn, frozen: list[str]) -> list[str]:
    params = list(inspect.signature(fn).parameters.values())[len(frozen) :]
    return [
        p.name
        for p in params
        if p.default is inspect.Parameter.empty and p.kind not in (p.VAR_POSITIONAL, p.VAR_KEYWORD)
    ]


@pytest.mark.parametrize(
    ("owner", "name"),
    [(m, n) for m, names in FUNCTIONS.items() for n in names] + list(METHODS),
    ids=lambda x: getattr(x, "__name__", x),
)
def test_parameters_beyond_the_frozen_prefix_have_defaults(owner, name):
    frozen = FUNCTIONS[owner][name] if owner in FUNCTIONS and name in FUNCTIONS[owner] else METHODS[(owner, name)]
    assert _extra_parameters_without_default(getattr(owner, name), frozen) == []


@pytest.mark.parametrize(("owner", "name"), list(ADDED_PARAMETERS), ids=lambda x: getattr(x, "__name__", x))
def test_added_parameters_are_keyword_only_with_their_default(owner, name):
    params = inspect.signature(getattr(owner, name)).parameters
    for param, default in ADDED_PARAMETERS[(owner, name)].items():
        assert params[param].kind is inspect.Parameter.KEYWORD_ONLY
        assert params[param].default is default or params[param].default == default
    assert inspect.signature(jsonutil.object_manifest).parameters["block_size"].default == jsonutil.OBJECT_BLOCK_SIZE


@pytest.mark.parametrize("cls", list(DATACLASS_FIELDS), ids=lambda c: c.__name__)
def test_fields_beyond_the_frozen_list_have_defaults(cls):
    extra = dataclasses.fields(cls)[len(DATACLASS_FIELDS[cls]) :]
    missing = [f.name for f in extra if f.default is dataclasses.MISSING and f.default_factory is dataclasses.MISSING]
    assert missing == []


@pytest.mark.parametrize("cls", list(ADDED_FIELDS), ids=lambda c: c.__name__)
def test_added_fields_follow_the_frozen_ones_with_their_default(cls):
    fields = {f.name: f for f in dataclasses.fields(cls)}
    names = list(fields)
    frozen = DATACLASS_FIELDS[cls]
    for name, default in ADDED_FIELDS[cls].items():
        assert names.index(name) >= len(frozen)
        assert fields[name].default == default
        assert fields[name].metadata.get(model.WIRE_OMIT_DEFAULT) is True


@pytest.mark.parametrize(
    "module", [model, errors, contracts, jsonutil, ids, paths, ignore, settings, policy, hostcompat]
)
def test_every_public_callable_has_a_docstring(module):
    missing = []
    for name, obj in vars(module).items():
        if name.startswith("_") or getattr(obj, "__module__", None) != module.__name__:
            continue
        if inspect.isfunction(obj) or inspect.isclass(obj):
            if not inspect.getdoc(obj):
                missing.append(name)
            if inspect.isclass(obj):
                for attr, member in vars(obj).items():
                    if inspect.isfunction(member) and not attr.startswith("_") and not inspect.getdoc(member):
                        missing.append(f"{name}.{attr}")
    assert missing == []
