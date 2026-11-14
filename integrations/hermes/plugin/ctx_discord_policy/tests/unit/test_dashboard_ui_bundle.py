"""The generated Desktop bundle (SETTINGS-PAGE §5.7): current with its sources, inside the SDK surface.

``desktop/plugin.js`` is committed so users need no Node; these tests keep it equal to what
``tools/assemble_ui.py`` produces from ``ui/``, and re-state the Desktop admission rules locally (the contract
test against Hermes' own lint per ref lives with the contract tests). No Node is needed here.
"""

from __future__ import annotations

import importlib.util
import re
import shutil
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BUNDLE = ROOT / "desktop" / "plugin.js"
ALLOWED_SPECIFIERS = {"@hermes/plugin-sdk", "react", "react/jsx-runtime"}


def _assembler():
    spec = importlib.util.spec_from_file_location("dsp_assemble_ui", ROOT / "tools" / "assemble_ui.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture(scope="module")
def assemble():
    return _assembler()


@pytest.fixture(scope="module")
def bundle() -> str:
    return BUNDLE.read_text(encoding="utf-8")


# String literals are kept, comments blanked (lines preserved), like Hermes' admission lint does it.
_COMMENT = re.compile(
    r"(\"(?:[^\"\\\n]|\\.)*\"|'(?:[^'\\\n]|\\.)*'|`(?:[^`\\]|\\.)*`)"
    r"|/\*.*?\*/|(?<![:\w])//[^\n]*",
    re.DOTALL,
)


def _strip_comments(source: str) -> str:
    return _COMMENT.sub(lambda m: m.group(1) or "\n" * m.group(0).count("\n"), source)


def _line(text: str, offset: int) -> int:
    return text.count("\n", 0, offset) + 1


# ── the bundle is generated, current and deterministic ──────────────────────────────────────────────


def test_committed_bundle_equals_the_generated_one(assemble, bundle):
    assert assemble.build(ROOT)["desktop/plugin.js"] == bundle, "run: python tools/assemble_ui.py"
    assert assemble.stale(ROOT) == []


def test_generation_is_deterministic_and_names_its_sources(assemble, bundle):
    assert assemble.build(ROOT) == assemble.build(ROOT)
    header = bundle.split("\n", 6)[:5]
    assert header[0].startswith("// desktop/plugin.js") and "GENERATED" in header[0]
    assert "//   ui/head.desktop.js" in header and "//   ui/core.js" in header


def _copy_tree(tmp_path: Path) -> Path:
    for rel in ("ui/head.desktop.js", "ui/core.js", "desktop/plugin.js"):
        (tmp_path / rel).parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(ROOT / rel, tmp_path / rel)
    return tmp_path


def test_check_mode_fails_and_names_the_file_when_the_bundle_is_stale(assemble, tmp_path, capsys):
    root = _copy_tree(tmp_path)
    assert assemble.main(["--check", "--root", str(root)]) == 0
    with (root / "ui" / "core.js").open("a", encoding="utf-8") as fh:
        fh.write("// changed\n")
    assert assemble.main(["--check", "--root", str(root)]) == 1
    assert "desktop/plugin.js" in capsys.readouterr().err
    assert assemble.main(["--root", str(root)]) == 0
    assert assemble.main(["--check", "--root", str(root)]) == 0
    (root / "desktop" / "plugin.js").unlink()
    assert assemble.main(["--check", "--root", str(root)]) == 1


def test_the_core_may_not_carry_module_statements(assemble, tmp_path, capsys):
    root = _copy_tree(tmp_path)
    core = root / "ui" / "core.js"
    core.write_text("import x from 'react'\n" + core.read_text(encoding="utf-8"), encoding="utf-8")
    assert assemble.main(["--root", str(root)]) == 2
    assert "ui/core.js:1" in capsys.readouterr().err


def test_line_endings_do_not_change_the_output(assemble, tmp_path):
    root = _copy_tree(tmp_path)
    for rel in ("ui/head.desktop.js", "ui/core.js"):
        path = root / rel
        path.write_bytes(path.read_bytes().replace(b"\n", b"\r\n"))
    assert assemble.build(root) == assemble.build(ROOT)


# ── the bundle stays inside the Desktop SDK surface ─────────────────────────────────────────────────


def test_import_specifiers_are_only_the_sdk_and_react(bundle):
    # The runtime loader's specifier pattern, applied to code *and* strings: stricter than the loader,
    # so no UI text can ever look like an import to any loader version.
    pattern = re.compile(r"(from\s*|import\s*\(\s*|import\s+)(['\"])([^'\"]+)\2")
    found = {m.group(3) for m in pattern.finditer(_strip_comments(bundle))}
    assert found and found <= ALLOWED_SPECIFIERS, found
    statements = re.findall(r"^import\b.*$", bundle, re.MULTILINE)
    assert statements == ["import * as SDK from '@hermes/plugin-sdk'", "import * as React from 'react'"]
    assert len(re.findall(r"^export\b", bundle, re.MULTILINE)) == 1


def test_default_export_id_is_the_plugin_id(bundle):
    match = re.search(r"^export default \{\s*id:\s*([A-Za-z_$][\w$]*|'[^']*')", bundle, re.MULTILINE)
    assert match, "export default { id: … } not found"
    value = match.group(1)
    if not value.startswith("'"):
        const = re.search(rf"^const {re.escape(value)} = '([^']*)'$", bundle, re.MULTILINE)
        assert const, f"{value} is not a string constant"
        value = const.group(1)
    assert value.strip("'") == "ctx_discord_policy"


def test_no_hardcoded_colours(bundle):
    code = _strip_comments(bundle)
    hits = []
    for name, pattern in (
        ("hex colour", r"(?<![\w&#])#(?:[0-9a-fA-F]{8}|[0-9a-fA-F]{6}|[0-9a-fA-F]{3,4})(?![\w-])"),
        ("rgb()/hsl()", r"\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\("),
        ("named colour", r"['\"](?:black|white|red|green|blue|gray|grey|yellow|orange|purple|pink|silver)['\"]"),
    ):
        hits += [(name, _line(code, m.start()), m.group(0)) for m in re.finditer(pattern, code)]
    assert hits == []
    assert "var(--ui-" in bundle


# Re-statement of hermes_cli/plugin_validate_desktop.py (refusals and review cues); the contract test runs
# Hermes' own copy per ref.
_FORBIDDEN = (
    ("dynamic code evaluation", re.compile(r"(?<![\w$.])eval\(|\bnew\s+Function\(")),
    ("dynamic import outside the SDK",
     re.compile(r"\bimport\(\s*(?!['\"](?:@hermes/plugin-sdk|react)(?:/[\w/-]*)?['\"]\s*\))")),
    ("remote import outside the SDK", re.compile(r"\bimport\s+(?:[^;'\"]*?\bfrom\s*)?['\"][a-zA-Z][\w+.-]*:")),
    ("script injection", re.compile(r"createElement\(\s*['\"]script['\"]\s*\)|<script\b")),
    ("app DOM reach", re.compile(r"\bdocument\.(?:querySelector(?:All)?|getElementsBy\w+|getElementById)\(\s*[`'\"][^`'\"]*"
                                 r"\[data-(?:slot|tour|sidebar|testid)\b")),
    ("app DOM reach", re.compile(r"\.observe\(\s*document\.body\s*,\s*\{[^}]*\b(?:childList|subtree)\b")),
    ("preview guest reach",
     re.compile(r"\b(?:querySelector(?:All)?|closest|getElementsByTagName|createElement)\(\s*[`'\"]webview\b")),
    ("raw bridge window.hermesDesktop", re.compile(r"\b(?:window|globalThis|self)\s*(?:\?\.|\.)\s*hermesDesktop\b")),
    ("app markup selector",
     re.compile(r"\[data-(?:slot|tour|sidebar|testid|tree-[\w-]+|pane-host|titlebar-cluster)\b")),
)


def test_local_restatement_of_the_desktop_lint_finds_nothing(bundle):
    code = _strip_comments(bundle)
    hits = [(rule, _line(code, m.start())) for rule, pattern in _FORBIDDEN for m in pattern.finditer(code)]
    assert hits == []


def test_untracked_timers_and_listeners_are_not_used(bundle):
    """Bare window timers/listeners survive a plugin reload; the core gets ctx's tracked ones."""
    code = _strip_comments(bundle)
    assert not re.search(r"(?<![\w.])(?:window\.)?set(?:Interval|Timeout)\(", code)
    assert not re.search(r"\bwindow\.addEventListener\(", code)


def test_only_the_bundle_lives_under_desktop():
    """Hermes lints every *.js under desktop/: sources stay in ui/."""
    files = sorted(p.relative_to(ROOT).as_posix() for p in (ROOT / "desktop").rglob("*") if p.is_file())
    assert files == ["desktop/plugin.js"]


def test_lint_restatement_catches_what_it_should():
    """Mutant guard: each restated rule fires on a minimal offending line."""
    samples = {
        "dynamic code evaluation": "eval('1')",
        "dynamic import outside the SDK": "import('left-pad')",
        "remote import outside the SDK": "import x from 'https://e.example/x.js'",
        "script injection": "document.createElement('script')",
        "raw bridge window.hermesDesktop": "window.hermesDesktop.fs",
        "app markup selector": "x.querySelector('[data-slot=input]')",
    }
    for rule, sample in samples.items():
        assert any(r == rule and p.search(sample) for r, p in _FORBIDDEN), rule
