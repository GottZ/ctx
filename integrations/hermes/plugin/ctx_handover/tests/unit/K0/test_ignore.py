"""K0: built-in ignores and the gitignore subset (DESIGN §8.2 step 5, §9.6) — real trees in tmp."""

from __future__ import annotations

import os
import subprocess

import pytest

from ctx_handover.engine.ignore import BUILTIN_IGNORES, MAX_IGNORE_FILE_BYTES, IgnoreMatcher


@pytest.mark.parametrize(
    ("rel", "is_dir"),
    [
        (".git", True),
        (".git", False),
        ("sub/.git", True),
        (".hg", True),
        ("node_modules", True),
        ("a/node_modules", True),
        ("pkg/__pycache__", True),
        ("x/mod.pyc", False),
        (".venv", True),
        ("venv", True),
        ("dist", True),
        ("build", True),
        ("target", True),
        (".gradle", True),
        (".next", True),
        (".cache", True),
        (".DS_Store", False),
        ("a/Thumbs.db", False),
        ("node_modules/left-pad/index.js", False),
    ],
)
def test_builtin_ignores(rel, is_dir):
    assert IgnoreMatcher([]).ignored(rel, is_dir)


@pytest.mark.parametrize(
    ("rel", "is_dir"), [("dist", False), ("src/build.py", False), ("venv.txt", False), ("a", True)]
)
def test_builtin_dir_only_and_exact(rel, is_dir):
    assert not IgnoreMatcher([]).ignored(rel, is_dir)


def test_builtins_cannot_be_negated(tmp_path):
    (tmp_path / ".gitignore").write_text("!node_modules/\n!.git\n")
    m = IgnoreMatcher.for_tree(tmp_path, ["!dist/"])
    assert m.ignored("node_modules", True) and m.ignored(".git", True) and m.ignored("dist", True)


def test_gitignore_subset_semantics(tmp_path):
    (tmp_path / ".gitignore").write_text(
        "\n".join(
            [
                "# comment",
                "",
                "*.log",
                "!keep.log",
                "/rooted.txt",
                "docs/*.md",
                "cache/",
                "**/gen/**",
                "a/**/b",
                "\\#hash",
                "\\!bang",
                "trail\\ ",
                "spaces   ",
                "[Tt]emp?",
                "[!a]x",
            ]
        )
        + "\n"
    )
    m = IgnoreMatcher.for_tree(tmp_path, [])
    expect = {
        ("x.log", False): True,
        ("deep/y.log", False): True,
        ("keep.log", False): False,
        ("deep/keep.log", False): False,
        ("rooted.txt", False): True,
        ("sub/rooted.txt", False): False,
        ("docs/a.md", False): True,
        ("docs/sub/a.md", False): False,
        ("other/docs/a.md", False): False,
        ("cache", True): True,
        ("cache", False): False,
        ("x/cache/y", False): True,
        ("p/gen/q/r.txt", False): True,
        ("gen", True): False,
        ("a/b", False): True,
        ("a/x/y/b", False): True,
        ("#hash", False): True,
        ("!bang", False): True,
        ("trail ", False): True,
        ("spaces", False): True,
        ("Temp1", False): True,
        ("temp22", False): False,
        ("bx", False): True,
        ("ax", False): False,
        ("src/main.py", False): False,
    }
    got = {key: m.ignored(*key) for key in expect}
    assert got == expect


def test_parent_exclusion_cannot_be_reincluded(tmp_path):
    (tmp_path / ".gitignore").write_text("out/\n!out/keep.txt\n")
    m = IgnoreMatcher.for_tree(tmp_path, [])
    assert m.ignored("out/keep.txt", False) and m.ignored("out/a/b.txt", False)


def test_nested_ignore_files_and_precedence(tmp_path):
    (tmp_path / "sub" / "deeper").mkdir(parents=True)
    (tmp_path / ".gitignore").write_text("*.tmp\n")
    (tmp_path / "sub" / ".gitignore").write_text("!keep.tmp\n/local.txt\n")
    (tmp_path / "sub" / ".handoverignore").write_text("big.bin\n")
    m = IgnoreMatcher.for_tree(tmp_path, ["*.secretlike"])
    assert m.ignored("a.tmp", False)
    assert not m.ignored("sub/keep.tmp", False)
    assert m.ignored("sub/other.tmp", False)
    assert m.ignored("sub/local.txt", False) and not m.ignored("sub/deeper/local.txt", False)
    assert m.ignored("sub/deeper/big.bin", False) and not m.ignored("big.bin", False)
    assert m.ignored("x/y.secretlike", False)


def test_matches_git_check_ignore_for_the_subset(tmp_path):
    """Differential check against git itself for patterns of the supported subset."""
    patterns = ["*.log", "!keep.log", "/rooted", "docs/*.md", "cache/", "**/gen/**", "a/**/b", "[Tt]emp?"]
    (tmp_path / ".gitignore").write_text("\n".join(patterns) + "\n")
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env.update({"GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_NOSYSTEM": "1"})
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True, env=env)  # noqa: S603,S607
    candidates = [
        "x.log",
        "keep.log",
        "d/keep.log",
        "rooted",
        "s/rooted",
        "docs/a.md",
        "docs/s/a.md",
        "cache/f",
        "p/gen/q/r",
        "a/b",
        "a/x/b",
        "Temp1",
        "temp1",
        "src/ok.py",
    ]
    proc = subprocess.run(  # noqa: S603
        ["git", "-C", str(tmp_path), "check-ignore", "--no-index", "--stdin"],  # noqa: S607
        input="\n".join(candidates) + "\n",
        capture_output=True,
        text=True,
        env=env,
        check=False,
    )
    git_ignored = set(proc.stdout.split())
    m = IgnoreMatcher.for_tree(tmp_path, [])
    ours = {c for c in candidates if m.ignored(c, False)}
    assert ours == git_ignored


def test_ignore_file_through_symlink_or_oversized_is_not_read(tmp_path):
    outside = tmp_path / "outside.txt"
    outside.write_text("*.py\n")
    root = tmp_path / "tree"
    root.mkdir()
    os.symlink(outside, root / ".gitignore")
    (root / ".handoverignore").write_bytes(b"*.md\n" + b"#" * MAX_IGNORE_FILE_BYTES)
    m = IgnoreMatcher.for_tree(root, [])
    assert not m.ignored("a.py", False) and not m.ignored("a.md", False)


def test_builtin_list_is_the_design_list():
    assert set(BUILTIN_IGNORES) >= {
        ".git",
        "node_modules/",
        "__pycache__/",
        ".venv/",
        "venv/",
        ".tox/",
        ".mypy_cache/",
        ".pytest_cache/",
        ".ruff_cache/",
        "dist/",
        "build/",
        "target/",
        ".gradle/",
        ".next/",
        ".cache/",
        "*.pyc",
        ".DS_Store",
        "Thumbs.db",
    }
