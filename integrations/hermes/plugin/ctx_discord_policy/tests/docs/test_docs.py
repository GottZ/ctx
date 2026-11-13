"""Repository hygiene: manifest/version consistency, license, and the private-value scan."""

from __future__ import annotations

import ast
import importlib.util
import os
import re
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def _load_yaml(text: str):
    try:
        import yaml  # PyYAML

        return yaml.safe_load(text)
    except ImportError:
        from ruamel.yaml import YAML

        return YAML(typ="safe").load(text)


def _module_constant(path: Path, name: str):
    tree = ast.parse(path.read_text(encoding="utf-8"))
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(isinstance(t, ast.Name) and t.id == name for t in node.targets):
            return ast.literal_eval(node.value)
    raise AssertionError(f"{name} not found in {path}")


def _pyproject_version() -> str:
    match = re.search(r'^version = "([^"]+)"', (ROOT / "pyproject.toml").read_text(encoding="utf-8"), re.M)
    assert match
    return match.group(1)


def test_manifest_fields():
    manifest = _load_yaml((ROOT / "plugin.yaml").read_text(encoding="utf-8"))
    assert manifest["manifest_version"] == 2
    assert manifest["name"] == "ctx_discord_policy"
    assert manifest["license"] == "MPL-2.0"
    assert manifest["author"] == "Jan-Stefan Janetzky"
    assert manifest["requires_hermes"].startswith(">=")
    assert set(manifest["provides_hooks"]) >= {"pre_llm_call", "pre_tool_call"}
    schema = manifest["config_schema"]
    assert schema["operators"]["type"] == "list" and schema["operators"]["default"] == []


def test_versions_agree():
    manifest = _load_yaml((ROOT / "plugin.yaml").read_text(encoding="utf-8"))
    shim = _module_constant(ROOT / "__init__.py", "PLUGIN_VERSION")
    package = _module_constant(ROOT / "dsp" / "__init__.py", "__version__")
    assert str(manifest["version"]) == shim == package == _pyproject_version()
    assert _module_constant(ROOT / "__init__.py", "RUNTIME_API") == _module_constant(
        ROOT / "dsp" / "__init__.py", "RUNTIME_API"
    )


def test_license_is_mpl_2_0():
    text = (ROOT / "LICENSE").read_text(encoding="utf-8")
    assert text.startswith("Mozilla Public License Version 2.0\n")
    assert "Exhibit A - Source Code Form License Notice" in text


def _scanner():
    name = "dsp_private_scan"
    spec = importlib.util.spec_from_file_location(name, ROOT / "ci" / "private_scan.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


KEY = b"synthetic-test-key-0123456789abcdef"
# Assembled at run time: the scanner's generic rule flags any literal UUID in the tree.
SYNTHETIC_UUID_PARTS = ("0123abcd", "4567", "89ab", "cdef", "0123456789ab")


def _patterns(scan, tmp_path, *entries, canary=True):
    lines = [scan.pattern_line(KEY, "key-check", "-")] if canary else []
    lines += [scan.pattern_line(KEY, kind, value) for kind, value in entries]
    path = tmp_path / "patterns.txt"
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return scan.load_patterns(path, key=KEY)


def test_pattern_file_is_keyed_and_complete():
    scan = _scanner()
    text = (ROOT / "ci" / "private_patterns.txt").read_text(encoding="utf-8")
    patterns = scan.load_patterns()  # validates every line
    assert patterns.count >= 23, "private patterns were removed (DESIGN 11.3 list)"
    assert sum(1 for line in text.splitlines() if line.startswith("key-check ")) == 1
    for line in text.splitlines():
        if line and not line.startswith("#"):
            assert scan.LINE_RE.match(line), line
    assert not re.search(r"(?<![0-9a-f])\d{15,21}(?![0-9a-f])", text), "plain snowflake in pattern file"


def test_wrong_key_is_refused(tmp_path):
    scan = _scanner()
    path = tmp_path / "patterns.txt"
    path.write_text(scan.pattern_line(KEY, "key-check", "-") + "\n", encoding="utf-8")
    with pytest.raises(scan.ScanError):
        scan.load_patterns(path, key=b"another-key-that-is-long-enough-000")
    path.write_text(scan.pattern_line(KEY, "word", "zebraquokka") + "\n", encoding="utf-8")
    with pytest.raises(scan.ScanError):  # no key-check line at all
        scan.load_patterns(path, key=KEY)


def test_scanner_finds_disguised_values(tmp_path):
    scan = _scanner()
    pats = _patterns(scan, tmp_path, ("word", "zebraquokka"), ("word", "zq"),
                     ("snowflake", "987654321098765432"), ("path", "/opt/zz-private"), ("date", "1999-12-31"),
                     ("uuid", "-".join(SYNTHETIC_UUID_PARTS)))
    hits = [
        "ZebraQuokkaBot", "myzebraquokka2", "zebra-quokka", "hermes_zebraquokka_x", "ＺｅｂｒａＱｕｏｋｋａ",
        "zq here", "ZqBot", "zq2", "id = 987_654_321_098_765_432", "x987654321098765432y",
        "987.654.321.098.765.432", "/opt//zz-private/sub", "on 1999-12-31", "31.12.1999", "19991231",
        "".join(SYNTHETIC_UUID_PARTS),
    ]
    for text in hits:
        assert scan.scan_text("t", text, KEY, pats, generic=False), text
    for text in ("zebra quokka", "zqx", "98765432109876543", "/opt/zz-public", "1999-12-30", "1.2.3"):
        assert not scan.scan_text("t", text, KEY, pats, generic=False), text


def test_hash_refuses_values_the_scanner_cannot_find():
    scan = _scanner()
    for kind, value in (("word", "Zebra Quokka"), ("snowflake", "abc"), ("path", "opt/zz"), ("word", "!!")):
        with pytest.raises(scan.ScanError):
            scan.pattern_line(KEY, kind, value)


def test_owner_is_allowed_only_inside_the_repository_slug(tmp_path, monkeypatch):
    scan = _scanner()
    monkeypatch.delenv("GITHUB_REPOSITORY_OWNER", raising=False)
    (tmp_path / "plugin.yaml").write_text(
        "homepage: https://github.com/OwnerName/ctx\n", encoding="utf-8")
    contexts = scan._owner_contexts(tmp_path)
    pats = _patterns(scan, tmp_path, ("word", "ownername"), ("word", "secretword"))
    assert not scan.scan_text("t", "https://github.com/OwnerName/ctx", KEY, pats,
                              contexts=contexts, generic=False)
    for text in ("operator OwnerName", "OwnerName/hermes-other", "ghcr.io/OwnerName/hermes-secretword",
                 "OwnerName/ctx-fork", "OwnerName/ctx.wiki"):
        assert scan.scan_text("t", text, KEY, pats, contexts=contexts, generic=False), text
    monkeypatch.setenv("GITHUB_REPOSITORY_OWNER", "SomeoneElse")
    assert scan._owner_contexts(tmp_path) == ()


def test_history_and_extra_text_are_scanned(tmp_path, monkeypatch):
    """A value added in one commit and removed in the next is still found (PR refs keep it)."""
    scan = _scanner()
    repo = tmp_path / "repo"
    (repo / "ci").mkdir(parents=True)
    key_file = tmp_path / "key"
    key_file.write_text(KEY.decode(), encoding="utf-8")
    (repo / "ci" / "private_patterns.txt").write_text(
        scan.pattern_line(KEY, "key-check", "-") + "\n" + scan.pattern_line(KEY, "word", "zebraquokka") + "\n",
        encoding="utf-8")
    # Two commits built with fast-import (a throwaway repository; no identity configuration involved).
    patterns_blob = (repo / "ci" / "private_patterns.txt").read_text(encoding="utf-8")

    def commit(mark, text, parent=None):
        body = [f"commit refs/heads/main", f"mark :{mark}", "committer Test <test@example.invalid> 0 +0000",
                "data 4", "test"]
        if parent:
            body.append(f"from :{parent}")
        for name, data in (("notes.txt", text), ("ci/private_patterns.txt", patterns_blob)):
            raw = data.encode("utf-8")
            body += [f"M 100644 inline {name}", f"data {len(raw)}", data]
        return "\n".join(body) + "\n"

    subprocess.run(["git", "init", "-q", str(repo)], check=True, capture_output=True)
    stream = commit(1, "the zebraquokka was here\n") + commit(2, "nothing to see\n", parent=1)
    subprocess.run(["git", "-C", str(repo), "fast-import", "--quiet"], input=stream.encode("utf-8"), check=True,
                   capture_output=True)
    subprocess.run(["git", "-C", str(repo), "checkout", "-q", "-f", "main"], check=True, capture_output=True)
    monkeypatch.setattr(scan, "PATTERNS", repo / "ci" / "private_patterns.txt")
    assert scan.main(["--root", str(repo), "--key-file", str(key_file)]) == 1
    assert scan.main(["--root", str(repo), "--key-file", str(key_file), "--no-history"]) == 0
    monkeypatch.setenv("PR_TEXT", "Title mentions ZebraQuokka")
    assert scan.main(["--root", str(repo), "--key-file", str(key_file), "--no-history",
                      "--text-env", "PR_TEXT"]) == 1


def test_scanner_refuses_to_run_without_key(monkeypatch):
    monkeypatch.delenv("DSP_PRIVATE_SCAN_KEY", raising=False)
    result = subprocess.run([sys.executable, str(ROOT / "ci" / "private_scan.py"), "--no-commits"],
                            capture_output=True, text=True, env={k: v for k, v in os.environ.items()
                                                                 if k != "DSP_PRIVATE_SCAN_KEY"})
    assert result.returncode == 2 and "key" in result.stderr


@pytest.mark.skipif(not os.environ.get("DSP_PRIVATE_SCAN_KEY"), reason="needs the scan key (CI secret)")
def test_private_scan_is_clean():
    result = subprocess.run([sys.executable, str(ROOT / "ci" / "private_scan.py"), "--root", str(ROOT)],
                            capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr
