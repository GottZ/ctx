"""Import discipline of the whole plugin tree (layer K0; INTERFACES §2, §3.6, BUILD-ORDER §0).

The checker parses every runtime ``.py`` file of the plugin (everything outside ``tests/``) with ``ast`` and
reports:

* an engine module that is not in the layer table (new modules are registered through the integrator),
* an engine import of the same or a higher layer (strictly downwards; INTERFACES §2),
* a relative import that leaves the engine package (the dashboard loads ``engine/`` standalone by path),
* an absolute import of the plugin's own package or of ``hermes_plugins`` from runtime code,
* a Hermes import anywhere except ``engine/hostcompat.py`` (also under ``TYPE_CHECKING``),
* a non-stdlib import in a stdlib-only module (third-party allowances: ``crypto`` → cryptography,
  ``wire``/``remote`` → httpx, ``dashboard/plugin_api.py`` → fastapi/starlette),
* a dynamic import (``importlib.import_module``/``__import__``) with a non-literal module name.

Imports under ``if TYPE_CHECKING:`` are exempt from the layer rule (they never run) but not from the Hermes
rule. Each rule is also exercised against synthetic trees, so the checker is proven before the cluster
modules exist.
"""

from __future__ import annotations

import ast
import json
import sys
from dataclasses import dataclass
from pathlib import Path

from ctx_handover.tests.conftest import PLUGIN_ROOT, require_upstream, upstream_root

LAYERS: dict[str, int] = {
    **dict.fromkeys(("model", "errors"), 0),
    **dict.fromkeys(("contracts", "jsonutil", "ids", "paths", "ignore"), 1),
    **dict.fromkeys(("policy", "session_hash", "blocks", "ledger", "crypto", "hostcompat"), 2),
    **dict.fromkeys(("gitops", "hermes_files", "ops", "continuation", "ctxrecords"), 3),
    **dict.fromkeys(
        (
            "gitpack",
            "gitapply",
            "filetree",
            "session_export",
            "session_import",
            "lock",
            "collector",
            "refine",
            "note",
            "pathmap",
            "envfacts",
        ),
        4,
    ),
    **dict.fromkeys(("materialize", "plan", "wire"), 5),
    **dict.fromkeys(("local_source", "local_target", "bundle", "remote", "pairing"), 6),
    **dict.fromkeys(("participant", "conductor"), 7),
    "server": 8,
    "jobs": 9,
    **dict.fromkeys(("surfaces", "progress"), 10),
    **dict.fromkeys(("commands", "agent_tool"), 11),
}
"""INTERFACES §2 (engine modules). ``../__init__.py`` and ``dashboard/plugin_api.py`` are layer 12."""

TOP_LAYER = 12

THIRD_PARTY_ALLOWED: dict[str, frozenset[str]] = {
    "engine/crypto.py": frozenset({"cryptography"}),
    "engine/wire.py": frozenset({"httpx"}),
    "engine/remote.py": frozenset({"httpx"}),
    "dashboard/plugin_api.py": frozenset({"fastapi", "starlette"}),
}

HERMES_IMPORTER = "engine/hostcompat.py"

# Hermes-owned top-level names known without a checkout (the checker adds the live list when Hermes is
# importable, the way ``hermes_cli/plugin_validate_core_override.py:_hermes_top_level`` derives it).
STATIC_HERMES_NAMES = frozenset(
    {
        "hermes_state",
        "hermes_constants",
        "hermes_cli",
        "hermes_platform",
        "hermes_plugins",
        "agent",
        "gateway",
        "tools",
        "plugins",
        "providers",
        "cron",
        "pm",
        "tui_gateway",
        "acp_adapter",
        "run_agent",
        "cli",
        "utils",
    }
)

EXTRA_STDLIB = frozenset({"__future__", "compression"})  # compression.zstd is stdlib from 3.14


@dataclass(frozen=True)
class Violation:
    file: str
    line: int
    rule: str
    detail: str

    def __str__(self) -> str:
        return f"{self.file}:{self.line} [{self.rule}] {self.detail}"


def hermes_names() -> frozenset[str]:
    names = set(STATIC_HERMES_NAMES)
    root = upstream_root()
    if root is not None:
        names |= {p.name for p in root.iterdir() if p.is_dir() and (p / "__init__.py").is_file()}
        names |= {p.stem for p in root.glob("*.py")}
        names -= {"setup", "conftest", "tests"}
    return frozenset(names)


def _runtime_files(root: Path) -> list[tuple[str, Path]]:
    out = []
    for path in sorted(root.rglob("*.py")):
        rel = path.relative_to(root)
        if rel.parts[0] == "tests" or any(p.startswith(".") or p == "__pycache__" for p in rel.parts[:-1]):
            continue
        out.append((rel.as_posix(), path))
    return out


def _type_checking_ids(tree: ast.AST) -> set[int]:
    skipped: set[int] = set()
    for node in ast.walk(tree):
        test = getattr(node, "test", None)
        if isinstance(node, ast.If) and (getattr(test, "id", None) or getattr(test, "attr", None)) == "TYPE_CHECKING":
            skipped |= {id(n) for stmt in node.body for n in ast.walk(stmt)}
    return skipped


def _layer_of(rel: str) -> int | None:
    if rel in {"__init__.py", "dashboard/plugin_api.py"}:
        return TOP_LAYER
    if rel.startswith("engine/") and rel.count("/") == 1:
        stem = rel[len("engine/") : -len(".py")]
        if stem == "__init__":
            return -1
        return LAYERS.get(stem)
    return None


def analyse(root: Path, *, package: str | None = None, hermes: frozenset[str] | None = None) -> list[Violation]:
    """All import-discipline violations of the plugin tree at ``root`` (see module docstring)."""
    package = package or root.name
    hermes = hermes if hermes is not None else hermes_names()
    stdlib = frozenset(sys.stdlib_module_names) | EXTRA_STDLIB
    engine_modules = {p.stem for p in (root / "engine").glob("*.py")} - {"__init__"}
    found: list[Violation] = []

    for rel, path in _runtime_files(root):
        layer = _layer_of(rel)
        if layer is None:
            found.append(Violation(rel, 0, "unregistered", "runtime module not in the INTERFACES §2 layer table"))
            continue
        try:
            tree = ast.parse(path.read_text(encoding="utf-8"), filename=rel)
        except SyntaxError as exc:
            found.append(Violation(rel, exc.lineno or 0, "syntax", str(exc)))
            continue
        skipped = _type_checking_ids(tree)
        in_engine = rel.startswith("engine/")
        allowed_third = THIRD_PARTY_ALLOWED.get(rel, frozenset())

        def engine_edge(target: str, node: ast.AST, guarded: bool, rel=rel, layer=layer) -> None:
            if target not in engine_modules and target not in LAYERS:
                found.append(Violation(rel, node.lineno, "unknown_engine_module", target))
                return
            if guarded:
                return
            target_layer = LAYERS.get(target)
            if target_layer is None:
                found.append(Violation(rel, node.lineno, "unregistered", f"imports unregistered engine.{target}"))
            elif target_layer >= layer:
                found.append(
                    Violation(rel, node.lineno, "layer", f"L{layer} imports engine.{target} (L{target_layer})")
                )

        def absolute(module: str, node: ast.AST, guarded: bool, rel=rel, allowed_third=allowed_third) -> None:
            top = module.split(".")[0]
            if top in {package, "hermes_plugins"}:
                found.append(Violation(rel, node.lineno, "self_absolute", f"absolute import of {module}"))
            elif top in hermes:
                if rel != HERMES_IMPORTER:
                    found.append(Violation(rel, node.lineno, "hermes_outside_hostcompat", module))
            elif top in stdlib:
                return
            elif not guarded and top not in allowed_third:
                found.append(Violation(rel, node.lineno, "non_stdlib", module))

        for node in ast.walk(tree):
            guarded = id(node) in skipped
            if isinstance(node, ast.Import):
                for alias in node.names:
                    absolute(alias.name, node, guarded)
            elif isinstance(node, ast.ImportFrom):
                if node.level == 0:
                    absolute(node.module or "", node, guarded)
                    continue
                if rel == "dashboard/plugin_api.py":
                    found.append(Violation(rel, node.lineno, "relative_in_dashboard", "loads the engine by path"))
                    continue
                if in_engine:
                    if node.level > 1:
                        found.append(Violation(rel, node.lineno, "escapes_engine", f"level {node.level} import"))
                        continue
                    if node.module:
                        engine_edge(node.module.split(".")[0], node, guarded)
                    else:
                        for alias in node.names:
                            if alias.name in engine_modules or alias.name in LAYERS:
                                engine_edge(alias.name, node, guarded)
                    continue
                # plugin root __init__.py: level 1 is the plugin package
                parts = (node.module or "").split(".")
                if node.level > 1:
                    found.append(Violation(rel, node.lineno, "escapes_plugin", f"level {node.level} import"))
                elif parts[0] == "engine" and len(parts) > 1:
                    engine_edge(parts[1], node, guarded)
                elif parts[0] == "engine":
                    for alias in node.names:
                        if alias.name in engine_modules or alias.name in LAYERS:
                            engine_edge(alias.name, node, guarded)
                elif parts[0] == "dashboard":
                    found.append(Violation(rel, node.lineno, "layer", "imports the dashboard module"))
            elif isinstance(node, ast.Call):
                func = ast.unparse(node.func)
                if func in {"importlib.import_module", "import_module", "__import__"}:
                    first = node.args[0] if node.args else None
                    if isinstance(first, ast.Constant) and isinstance(first.value, str):
                        absolute(first.value, node, guarded)
                    else:
                        found.append(Violation(rel, node.lineno, "dynamic_import", func))
    return found


def _rules(found: list[Violation]) -> set[str]:
    return {v.rule for v in found}


def _tree(tmp_path: Path, files: dict[str, str]) -> Path:
    root = tmp_path / "ctx_handover"
    for rel, text in {"__init__.py": "", "engine/__init__.py": "", **files}.items():
        target = root / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text, encoding="utf-8")
    return root


# ---------------------------------------------------------------------------------------------------------
# The real tree
# ---------------------------------------------------------------------------------------------------------


def test_layering_no_upward_or_same_layer_imports():
    """★ INTERFACES §2 on the real plugin tree [BX M11].

    Red proof: mutant = add ``engine/collector.py`` (empty) and ``engine/refine.py`` with ``from . import
    collector`` (same layer L4) → assertion lists ``[layer] L4 imports engine.collector (L4)``.
    """
    found = [v for v in analyse(PLUGIN_ROOT) if v.rule in {"layer", "escapes_engine", "escapes_plugin", "unregistered"}]
    assert found == [], "\n".join(map(str, found))


def test_stdlib_only_modules():
    """★ stdlib-only modules import no Hermes or third-party package (INTERFACES §2).

    Red proof: mutant = ``import yaml`` added to ``engine/paths.py`` → ``[non_stdlib] yaml``.
    """
    found = [v for v in analyse(PLUGIN_ROOT) if v.rule in {"non_stdlib", "dynamic_import", "self_absolute"}]
    assert found == [], "\n".join(map(str, found))


def test_only_hostcompat_imports_hermes():
    """★ Hermes imports outside ``engine/hostcompat.py`` fail (INTERFACES §3.6, catalog rule 9).

    Red proof: mutant = ``from hermes_state import SessionDB`` inside a function of ``engine/policy.py`` →
    ``[hermes_outside_hostcompat] hermes_state``.
    """
    found = [v for v in analyse(PLUGIN_ROOT) if v.rule == "hermes_outside_hostcompat"]
    assert found == [], "\n".join(map(str, found))


def test_whole_tree_clean():
    """Every rule at once on the real tree (catches rules the three ★ tests do not group)."""
    found = analyse(PLUGIN_ROOT)
    assert found == [], "\n".join(map(str, found))


def _public_api(root: Path) -> frozenset[str]:
    data = json.loads((root / "hermes_cli" / "data" / "plugin_api.json").read_text(encoding="utf-8-sig"))
    return frozenset(entry for group in data["groups"] for entry in group["entries"])


def test_public_imports_only():
    """Every Hermes import in ``hostcompat`` is in main's ``hermes_cli/data/plugin_api.json``, and the validator's
    own private-import and private-attribute passes report nothing for the plugin."""
    upstream = require_upstream()
    api = _public_api(upstream)
    hermes = hermes_names()
    tree = ast.parse((PLUGIN_ROOT / HERMES_IMPORTER).read_text(encoding="utf-8"))
    seen: list[str] = []
    for node in ast.walk(tree):
        pairs: list[tuple[str, str]] = []
        if isinstance(node, ast.Import):
            pairs = [(a.name, "") for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.level == 0 and node.module:
            pairs = [(node.module, a.name) for a in node.names]
        for module, name in pairs:
            if module.split(".")[0] not in hermes:
                continue
            target = f"{module}.{name}" if name else module
            public = target in api or f"{module}:{name}" in api or (module in api and not name.startswith("_"))
            assert public, f"{target} is not in plugin_api.json"
            seen.append(target)
    assert {"hermes_state.SessionDB", "agent.secret_scope.get_secret", "agent.plugin_llm.PluginLlm"} <= set(seen)

    from hermes_cli.plugin_validate_private_imports import private_attribute_findings, private_import_findings

    assert private_import_findings(PLUGIN_ROOT) == []
    assert private_attribute_findings(PLUGIN_ROOT) == []


def test_no_sys_modules_ctx_lookup():
    """No runtime file names ``hermes_plugins`` (no ``sys.modules["hermes_plugins…"]`` lookup of a ctx, SEC 7.2)."""
    hits = []
    for rel, path in _runtime_files(PLUGIN_ROOT):
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if isinstance(node, ast.Constant) and isinstance(node.value, str) and "hermes_plugins" in node.value:
                hits.append(f"{rel}:{node.lineno}")
    assert hits == []


def test_engine_init_imports_nothing_from_engine():
    """``engine/__init__.py`` stays import-free inside the engine (it is loaded first in both import modes)."""
    tree = ast.parse((PLUGIN_ROOT / "engine" / "__init__.py").read_text(encoding="utf-8"))
    relative = [n.lineno for n in ast.walk(tree) if isinstance(n, ast.ImportFrom) and n.level]
    assert relative == []


# ---------------------------------------------------------------------------------------------------------
# The checker on synthetic trees (each rule proven to fire)
# ---------------------------------------------------------------------------------------------------------


def test_checker_flags_same_layer_and_upward(tmp_path):
    root = _tree(
        tmp_path,
        {
            "engine/collector.py": "",
            "engine/refine.py": "from . import collector\n",
            "engine/model.py": "from .policy import Verdict\n",
            "engine/policy.py": "from .paths import check_rel\nfrom . import model\n",
            "engine/paths.py": "",
        },
    )
    found = analyse(root)
    layer = sorted(v.detail for v in found if v.rule == "layer")
    assert layer == ["L0 imports engine.policy (L2)", "L4 imports engine.collector (L4)"]


def test_checker_type_checking_exempt_from_layers_not_from_hermes(tmp_path):
    root = _tree(
        tmp_path,
        {
            "engine/blocks.py": (
                "from typing import TYPE_CHECKING\nif TYPE_CHECKING:\n    from .ledger import Ledger\n"
                "    from hermes_state import SessionDB\n"
            ),
            "engine/ledger.py": "",
        },
    )
    assert _rules(analyse(root)) == {"hermes_outside_hostcompat"}


def test_checker_flags_hermes_outside_hostcompat_and_allows_it_inside(tmp_path):
    root = _tree(
        tmp_path,
        {
            "engine/policy.py": "def f():\n    from hermes_state import SessionDB\n    return SessionDB\n",
            "engine/hostcompat.py": "def g():\n    import hermes_constants\n    return hermes_constants\n",
            "__init__.py": "import agent.redact\n",
        },
    )
    hits = sorted((v.file, v.rule) for v in analyse(root))
    assert hits == [("__init__.py", "hermes_outside_hostcompat"), ("engine/policy.py", "hermes_outside_hostcompat")]


def test_checker_flags_third_party_and_honours_allowances(tmp_path):
    root = _tree(
        tmp_path,
        {
            "engine/paths.py": "import yaml\nimport json\nfrom compression import zstd\n",
            "engine/crypto.py": "from cryptography.hazmat.primitives.asymmetric import ed25519\n",
            "engine/wire.py": "import httpx\n",
            "engine/blocks.py": "import httpx\n",
            "dashboard/plugin_api.py": "from fastapi import APIRouter\n",
        },
    )
    hits = sorted((v.file, v.detail) for v in analyse(root) if v.rule == "non_stdlib")
    assert hits == [("engine/blocks.py", "httpx"), ("engine/paths.py", "yaml")]


def test_checker_flags_escapes_dynamic_and_unregistered(tmp_path):
    root = _tree(
        tmp_path,
        {
            "engine/paths.py": "from .. import register\nimport importlib\nimportlib.import_module(name)\n",
            "engine/helpers.py": "",
            "engine/model.py": "import ctx_handover.engine.errors\n",
            "scripts/tool.py": "",
            "dashboard/plugin_api.py": "from ..engine import jobs\n",
        },
    )
    assert _rules(analyse(root)) == {
        "escapes_engine",
        "dynamic_import",
        "unregistered",
        "self_absolute",
        "relative_in_dashboard",
    }


def test_checker_plugin_root_may_import_any_engine_module(tmp_path):
    root = _tree(
        tmp_path,
        {
            "__init__.py": "from .engine import commands\nfrom .engine.lock import VetoIndex\n",
            "engine/commands.py": "from . import jobs\n",
            "engine/jobs.py": "from . import hostcompat\n",
            "engine/lock.py": "",
            "engine/hostcompat.py": "",
        },
    )
    assert analyse(root) == []


def test_layer_table_covers_interfaces_modules():
    """The table holds exactly the 45 engine modules of INTERFACES §1, by cluster (guards typos)."""
    by_cluster = {
        "K0": {"model", "errors", "contracts", "jsonutil", "ids", "paths", "ignore", "policy", "hostcompat"},
        "B": {"session_hash", "session_export", "session_import", "lock", "hermes_files"},
        "D": {"collector", "refine", "plan", "pathmap", "envfacts", "note"},
        "E": {"gitops", "gitpack", "gitapply", "filetree", "materialize"},
        "C": {"blocks", "crypto", "pairing", "wire", "server", "remote", "bundle"},
        "A": {"ledger", "ops", "local_source", "local_target", "participant", "conductor", "jobs"},
        "F": {"commands", "surfaces", "progress", "agent_tool", "ctxrecords", "continuation"},
    }
    expected = set().union(*by_cluster.values())
    assert set(LAYERS) == expected
    assert len(expected) == 45
