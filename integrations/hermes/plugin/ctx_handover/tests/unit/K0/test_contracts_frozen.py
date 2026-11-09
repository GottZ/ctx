"""K0: the frozen surface — every INTERFACES §3 name exists with its parameters (in order) and a docstring.

Additive rule: K0 may append keyword parameters with defaults after the specified ones; it never renames,
reorders or removes one. This test is the tripwire for that rule.
"""

from __future__ import annotations

import dataclasses
import inspect

import pytest

from ctx_handover.engine import contracts, errors, hostcompat, ids, ignore, jsonutil, model, paths, policy

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
    },
    jsonutil: {
        "canonical_bytes": ["obj"],
        "sha256_hex": ["data"],
        "doc_id": ["obj"],
        "loads_bounded": ["data", "max_bytes", "max_depth"],
        "to_wire": ["obj"],
        "from_wire": ["cls", "data"],
    },
    ids: {"ulid": [], "crockford": ["nbytes"], "short": ["job_id"], "mint_session_id": ["now"]},
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
    },
}

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
    model.Options: ["dest", "maps", "hints", "model", "force", "dry_run", "yes", "wait", "wait_timeout_s"],
    model.Hello: ["protocol", "majors", "minor", "instance_id", "listener_pubkey", "plugin_version"],
    model.EnvFacts: ["instance_name", "os_family", "os_name", "os_version", "arch", "hostname", "user", "home"],
    model.PlanItem: ["item_id", "kind", "source_path", "role", "origin", "include", "reasons", "size_estimate"],
    model.TargetDecision: ["item_id", "dest", "mode", "reason", "conflict", "git_have", "git_seed", "need_bytes"],
    model.CommitAck: ["session_id", "root_id", "title", "dests", "path_map", "model", "offline_capable"],
}

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


@pytest.mark.parametrize("module", [model, errors, contracts, jsonutil, ids, paths, ignore, policy, hostcompat])
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
