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
    assert str(manifest["version"]) == shim == package
    assert _module_constant(ROOT / "__init__.py", "RUNTIME_API") == _module_constant(
        ROOT / "dsp" / "__init__.py", "RUNTIME_API"
    )


def test_license_is_mpl_2_0():
    text = (ROOT / "LICENSE").read_text(encoding="utf-8")
    assert text.startswith("Mozilla Public License Version 2.0\n")
    assert "Exhibit A - Source Code Form License Notice" in text


# Directories a test run or a local checkout leaves next to the tracked files (used without git only).
_UNTRACKED_DIRS = {"node_modules", "__pycache__", ".pytest_cache", "contract-out"}


def _tree_files() -> list[str]:
    """The plugin's files: what git tracks here (the private-value scan's list), or (no checkout) every file
    outside the untracked dirs."""
    scan = _scanner()
    try:
        names = scan.tracked_files(ROOT)
    except scan.ScanError:
        names = []
    return names or [p.relative_to(ROOT).as_posix() for p in ROOT.rglob("*")
                     if p.is_file() and not _UNTRACKED_DIRS & set(p.relative_to(ROOT).parts)]


def _hidden(char: str) -> bool:
    """Format characters (zero-width, joiners, byte order mark, bidirectional controls, tag characters) and
    control characters other than tab, line feed and carriage return: none of them shows in an editor or a
    diff."""
    import unicodedata

    category = unicodedata.category(char)
    return category == "Cf" or (category == "Cc" and char not in "\t\n\r")


def test_no_invisible_characters_in_the_tree():
    """Every such character is written as an escape sequence. Hermes' install scanner rates a literal one as
    text hiding (high), which turns ``hermes plugins install`` into a confirmation prompt, refused without a
    terminal; and nobody reviewing the file can see it."""
    import unicodedata

    found = []
    for name in _tree_files():
        path = ROOT / name
        if path.is_symlink() or not path.is_file():
            continue
        try:
            text = path.read_bytes().decode("utf-8")
        except UnicodeDecodeError:
            continue  # binary (the banner, images): not source
        for number, line in enumerate(text.split("\n"), 1):
            for char in sorted({c for c in line if _hidden(c)}):
                found.append(f"{name}:{number}: U+{ord(char):04X} {unicodedata.name(char, '')}")
    assert not found, "write these as escape sequences:\n" + "\n".join(found)


def _readme_section(title: str) -> str:
    text = (ROOT / "README.md").read_text(encoding="utf-8")
    match = re.search(rf"^## {re.escape(title)}\n(.*?)(?=^## |\Z)", text, re.M | re.S)
    assert match, f"README has no section {title!r}"
    return match.group(1)


def test_install_is_named_for_every_profile():
    """A Hermes profile finds plugins only in its own home (tests/contract/test_install.py), so enabling the
    plugin in a profile it was never installed in fails ("No plugin named ..."): the README installs it there."""
    section = _readme_section("Install")
    main = re.search(r"^hermes plugins install (\S+) --enable$", section, re.M)
    assert main, "the install command"
    assert f"\nhermes -p <profile> plugins install {main.group(1)} --enable\n" in section
    assert "plugins enable ctx_discord_policy" not in section


def test_readme_says_what_host_isolation_leaves():
    """Under host isolation Hermes skips plugin commands (tests/contract/test_status_contract.py): no status
    can say anything there, so the README names the command's absence and Hermes' own view instead."""
    section = _readme_section("Before you start")
    assert "says so in the status" not in section
    assert "there is no `hermes ctx-discord-policy` command" in section
    assert "`hermes plugins info ctx_discord_policy`" in section


def test_readme_links_nothing_above_the_plugin_directory():
    """An installed copy is the plugin directory alone: a relative path out of it (the banner) leads nowhere
    there, and Hermes' install scanner reports it as a path traversal."""
    text = (ROOT / "README.md").read_text(encoding="utf-8")
    assert not re.findall(r"""(?:\]\(|src=["']|href=["'])\.\./""", text)


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


def _git_repo(repo: Path, stream: str) -> None:
    """A throwaway repository from a fast-import stream (no identity configuration involved)."""
    subprocess.run(["git", "init", "-q", str(repo)], check=True, capture_output=True)
    subprocess.run(["git", "-C", str(repo), "fast-import", "--quiet"], input=stream.encode("utf-8"), check=True,
                   capture_output=True)


def _checkout(repo: Path, branch: str) -> None:
    subprocess.run(["git", "-C", str(repo), "checkout", "-q", "-f", branch], check=True, capture_output=True)


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

    _git_repo(repo, commit(1, "the zebraquokka was here\n") + commit(2, "nothing to see\n", parent=1))
    _checkout(repo, "main")
    monkeypatch.setattr(scan, "PATTERNS", repo / "ci" / "private_patterns.txt")
    assert scan.main(["--root", str(repo), "--key-file", str(key_file)]) == 1
    assert scan.main(["--root", str(repo), "--key-file", str(key_file), "--no-history"]) == 0
    monkeypatch.setenv("PR_TEXT", "Title mentions ZebraQuokka")
    assert scan.main(["--root", str(repo), "--key-file", str(key_file), "--no-history",
                      "--text-env", "PR_TEXT"]) == 1


def test_scan_inside_a_larger_repository_covers_the_plugin_directory_only(tmp_path, monkeypatch):
    """In the ctx repository the plugin lives in a subdirectory: its files, their history and the messages of
    the commits that touch it are scanned; files elsewhere and commits that touch only other paths are not."""
    scan = _scanner()
    repo = tmp_path / "repo"
    plugin = repo / "integrations" / "plug"
    key_file = tmp_path / "key"
    key_file.write_text(KEY.decode(), encoding="utf-8")
    patterns = scan.pattern_line(KEY, "key-check", "-") + "\n" + scan.pattern_line(KEY, "word", "zebraquokka") + "\n"
    steps = (  # (branch, message, {path: text}); each branch continues the one before it
        ("a", "start", {"integrations/plug/ci/private_patterns.txt": patterns,
                        "integrations/plug/notes.txt": "nothing to see\n", "elsewhere.txt": "a zebraquokka\n"}),
        ("b", "the zebraquokka elsewhere", {"elsewhere.txt": "the zebraquokka again\n"}),
        ("c", "notes", {"integrations/plug/notes.txt": "a zebraquokka inside\n"}),
        ("d", "clean notes", {"integrations/plug/notes.txt": "nothing to see\n"}),
        ("e", "mentions ZebraQuokka", {"integrations/plug/notes.txt": "still nothing\n"}),
    )
    stream = []
    for mark, (branch, message, files) in enumerate(steps, 1):
        body = [f"commit refs/heads/{branch}", f"mark :{mark}", "committer Test <test@example.invalid> 0 +0000",
                f"data {len(message.encode())}", message]
        if mark > 1:
            body.append(f"from :{mark - 1}")
        for name, data in files.items():
            raw = data.encode("utf-8")
            body += [f"M 100644 inline {name}", f"data {len(raw)}", data]
        stream.append("\n".join(body) + "\n")
    _git_repo(repo, "".join(stream))
    monkeypatch.setattr(scan, "PATTERNS", plugin / "ci" / "private_patterns.txt")

    def run(branch, *extra):
        _checkout(repo, branch)
        return scan.main(["--root", str(plugin), "--key-file", str(key_file), *extra])

    assert run("b") == 0                    # the word only outside the plugin, in a file and a commit message
    assert scan.repo_prefix(plugin) == "integrations/plug/"
    assert run("c", "--no-history") == 1    # now inside the plugin's tree
    assert run("d", "--no-history") == 0
    assert run("d") == 1                    # an earlier version of a plugin file still carries it
    assert run("e", "--no-history") == 1    # message of a commit that touches the plugin
    assert scan.repo_prefix(repo) == "" and scan._scope(repo) == []


def test_files_of_the_plugin_outside_its_directory_are_scanned_with_also(tmp_path, monkeypatch):
    """The plugin owns files outside its directory (its workflows, its banner): ``--also`` adds each of them
    (path from the repository top) to the scan: the file, its earlier versions and the messages of commits that
    touch it. A path without tracked files is an error, never a silent pass."""
    scan = _scanner()
    repo = tmp_path / "repo"
    plugin = repo / "integrations" / "plug"
    key_file = tmp_path / "key"
    key_file.write_text(KEY.decode(), encoding="utf-8")
    patterns = scan.pattern_line(KEY, "key-check", "-") + "\n" + scan.pattern_line(KEY, "word", "zebraquokka") + "\n"
    workflow, banner = ".github/workflows/plug.yml", "assets/plug.webp"
    steps = (  # (branch, message, {path: text}); each branch continues the one before it
        ("a", "start", {"integrations/plug/ci/private_patterns.txt": patterns,
                        "integrations/plug/notes.txt": "nothing to see\n", workflow: "on: push\n",
                        banner: "RIFF....WEBP"}),
        ("b", "workflow", {workflow: "name: zebraquokka\n"}),
        ("c", "clean workflow", {workflow: "on: push\n"}),
        ("d", "the zebraquokka in a message", {workflow: "on: pull_request\n"}),
        ("e", "banner", {banner: "RIFF....WEBPEXIF artist=ZebraQuokka"}),
    )
    stream = []
    for mark, (branch, message, files) in enumerate(steps, 1):
        body = [f"commit refs/heads/{branch}", f"mark :{mark}", "committer Test <test@example.invalid> 0 +0000",
                f"data {len(message.encode())}", message]
        if mark > 1:
            body.append(f"from :{mark - 1}")
        for name, data in files.items():
            raw = data.encode("utf-8")
            body += [f"M 100644 inline {name}", f"data {len(raw)}", data]
        stream.append("\n".join(body) + "\n")
    _git_repo(repo, "".join(stream))
    monkeypatch.setattr(scan, "PATTERNS", plugin / "ci" / "private_patterns.txt")

    def run(branch, *extra):
        _checkout(repo, branch)
        return scan.main(["--root", str(plugin), "--key-file", str(key_file), *extra])

    also = ("--also", workflow, "--also", banner)
    assert run("b") == 0 and run("b", *also) == 1                            # the file itself
    assert run("c", "--no-commits") == 0 and run("c", "--no-commits", *also) == 1  # an earlier version of it
    assert run("d", "--no-history") == 0 and run("d", "--no-history", *also) == 1  # a commit touching it
    assert run("e", "--no-history", "--no-commits", "--also", banner) == 1  # metadata in the banner
    assert run("e", "--no-history", "--no-commits", "--also", workflow) == 0
    assert run("e", "--also", "assets/missing.webp") == 2


def test_binary_files_are_scanned_whole_and_short_words_only_in_their_text_fields(tmp_path):
    """An image is read whole (latin-1) for every rule but the short words: its compressed data holds short letter
    pairs at random, which the short-word rule would report. Short words are looked for in its text fields:
    printable ASCII runs of six characters or more, and UTF-16 runs."""
    scan = _scanner()
    pats = _patterns(scan, tmp_path, ("word", "zq"), ("word", "zebraquokka"), ("word", "quok"))

    def hits(name, data):
        return [f for text, short in scan.blob_texts(name, data)
                for f in scan.scan_text(name, text, KEY, pats, generic=False, short_words=short)]

    assert hits("notes.txt", b"\x00\x01zq\x02")                          # text: every token counts
    assert not hits("banner.webp", b"RIFF\x00\x01zq\x02\x9cZq\x00")       # chance bytes in image data
    assert hits("banner.webp", b"RIFF\x00\x00XMP artist zq\x00")          # an ASCII text field
    assert hits("banner.webp", b"RIFF\x00XPAuthor\x00\x00" + "zq".encode("utf-16-le") + b"\x00\x00")  # EXIF XP*
    assert hits("doc.pdf", b"/Author (\xfe\xff" + "zq".encode("utf-16-be") + b")")
    assert hits("banner.webp", b"RIFF\x00\x00quok\x00")                   # four letters and more: anywhere
    assert hits("banner.webp", b"RIFF\x00EXIF\x00ZebraQuokka\x00")
    assert hits("banner.webp", "z\x00e\x00b\x00r\x00a\x00q\x00u\x00o\x00k\x00k\x00a\x00".encode("latin-1"))


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
