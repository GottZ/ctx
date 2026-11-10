"""K0: path normalisation and safety rules (DESIGN §10.3–§10.5, TESTPLAN §2.1) — real filesystem in tmp."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import paths
from ctx_handover.engine.errors import BadRequest, PolicyDenied

LINUX = paths.rules_for("linux", False)
MAC = paths.rules_for("darwin", True)
WIN = paths.rules_for("windows", True)


@pytest.mark.parametrize(
    "rel",
    [
        "",
        "..",
        "a/../b",
        "../etc/passwd",
        "./a",
        "a/./b",
        "a//b",
        "a/",
        "/etc/passwd",
        "\\\\srv\\share",
        "C:/x",
        "a/\x00b",
        "a/\nb",
        "a/\x7fb",
        "a\u0085b",
        ".git",
        ".GIT/config",
        "sub/.Git/hooks/post-checkout",
        "git~1/config",
        "GIT~1",
        ".g\u200cit/config",
        ".\u200dgit",
        "\ufeff.git",
        ".git./x",
        ".git /x",
        ".git::$INDEX_ALLOCATION/x",
        "x" * 256,
        "a/" + "é" * 128,
        "/".join(["d" * 200] * 21),
    ],
)
@pytest.mark.parametrize("rules", [LINUX, MAC, WIN], ids=["linux", "mac", "windows"])
def test_check_rel_rejects_traversal(rel, rules):
    """★ Traversal, absolute, control characters, over-long and every ``.git`` spelling (incl. the HFS-ignorable
    U+200C on macOS rules, SEC 2.1) are rejected on every target.

    Red proof: mutant = ``is_dotgit`` without ``strip_hfs_ignorable`` (``name = component``) →
    ``Failed: DID NOT RAISE`` for ``.g\\u200cit/config``.
    """
    with pytest.raises(PolicyDenied):
        paths.check_rel(rel, rules)


@pytest.mark.parametrize("rel", ["a", "a/b/c.txt", "ünïcode/ñame", "a b/c d", ".gitignore", ".github/x", "x" * 255])
def test_check_rel_accepts_and_returns_unchanged(rel):
    assert paths.check_rel(rel, LINUX) == rel
    assert paths.check_rel(rel, MAC) == rel


@pytest.mark.parametrize(
    "rel",
    [
        "a\\b",
        "a:b",
        "CON",
        "con.txt",
        "dir/Nul.tar.gz",
        "COM1",
        "lpt9.log",
        "COM\u00b9",
        "aux ",
        "name.",
        "a/b?",
        "a*b",
        'q"x',
        "p|q",
        "<x>",
    ],
)
def test_check_rel_windows_only_rules(rel):
    """Backslash, ``:``, reserved device names, trailing dot/space and NTFS-invalid characters are refused only
    under Windows rules; Linux accepts ``a:b`` [BX M13]."""
    with pytest.raises(PolicyDenied):
        paths.check_rel(rel, WIN)
    if rel not in {"aux ", "name."}:
        assert paths.check_rel(rel, LINUX) == rel
    assert paths.check_rel("console/coms.txt", WIN) == "console/coms.txt"


def test_check_rel_windows_length_limit_and_long_paths():
    rel = "/".join(["d" * 50] * 6)
    assert len(rel) > 260
    with pytest.raises(PolicyDenied):
        paths.check_rel(rel, WIN)
    assert paths.check_rel(rel, paths.rules_for("windows", True, long_paths=True)) == rel
    assert paths.check_rel(rel, LINUX) == rel


@pytest.mark.parametrize(
    "path", ["\\\\srv\\share\\x", "\\\\?\\C:\\x", "\\\\.\\pipe\\x", "\\??\\C:\\x", "//srv/share/x", "//?/C:/x", "/\\x"]
)
def test_reject_windows_device_prefix(path, monkeypatch, tmp_path):
    """★ UNC and device paths are rejected before any filesystem call [SEC 2.2] — directly and through the policy
    entry points that take a path.

    Red proof: mutant = delete the ``reject_windows_device_prefix(raw)`` block in ``policy.check_source_file`` →
    the lstat spy raises ``AssertionError: filesystem touched for //srv/share/x``.
    """
    with pytest.raises(PolicyDenied):
        paths.reject_windows_device_prefix(path)

    from ctx_handover.engine import policy
    from ctx_handover.engine.model import Options

    cfg = policy.PolicyConfig.from_options(
        Options(),
        home=tmp_path,
        hermes_home=tmp_path / "hh",
        hermes_root=tmp_path / "hh",
        plugin_dir=tmp_path / "hh" / "pd",
        case_insensitive=False,
        target_os="linux",
    )

    def no_fs(*a, **kw):
        raise AssertionError(f"filesystem touched for {path}")

    with monkeypatch.context() as m:  # restored before pytest reports, which needs the filesystem itself
        for name in ("lstat", "stat", "open"):
            m.setattr(os, name, no_fs)
        m.setattr(os.path, "realpath", no_fs)
        source = policy.check_source_file(Path(path), cfg, explicit=True)
        target = policy.check_target_dest(Path(path), cfg, recorded=True, local_choice=True)
        with pytest.raises(PolicyDenied):
            paths.expand_root(path, tmp_path)
    assert source == policy.Verdict(False, "special")
    assert target == policy.Verdict(False, "target_denied")


def test_safe_join_rejects_symlinked_and_file_parents(tmp_path):
    root = tmp_path / "root"
    (root / "real").mkdir(parents=True)
    (tmp_path / "outside").mkdir()
    os.symlink(tmp_path / "outside", root / "link")
    (root / "file").write_text("x")
    assert paths.safe_join(root, "real/new/deeper.txt", LINUX) == root / "real" / "new" / "deeper.txt"
    assert paths.safe_join(root, "link", LINUX) == root / "link"  # final component is the writer's job
    with pytest.raises(PolicyDenied, match="symlink_component"):
        paths.safe_join(root, "link/x.txt", LINUX)
    with pytest.raises(PolicyDenied, match="not_a_directory"):
        paths.safe_join(root, "file/x.txt", LINUX)
    with pytest.raises(PolicyDenied):
        paths.safe_join(Path("relative"), "x", LINUX)
    with pytest.raises(PolicyDenied):
        paths.safe_join(root, "../x", LINUX)


def test_under_any_lexical_casefold_and_dotdot(tmp_path):
    root = tmp_path / "Root"
    assert paths.under_any(root, [root]) and paths.under_any(root / "a" / "b", [tmp_path / "x", root])
    assert not paths.under_any(tmp_path / "Rootx", [root])
    assert not paths.under_any(tmp_path / "root" / "a", [root])
    assert paths.under_any(tmp_path / "root" / "a", [root], case_insensitive=True)
    assert not paths.under_any(root / ".." / "x", [tmp_path])
    assert not paths.under_any(Path("rel"), [Path("rel")])


def test_expand_root(tmp_path):
    home = tmp_path / "home"
    assert paths.expand_root("~", home) == home
    assert paths.expand_root("~/hermes-handover", home) == home / "hermes-handover"
    assert paths.expand_root("work/x/../y", home) == home / "work" / "y"
    assert paths.expand_root(str(tmp_path / "abs"), home) == tmp_path / "abs"
    for bad in ("", "  ", "~root/x"):
        with pytest.raises(PolicyDenied):
            paths.expand_root(bad, home)


@pytest.mark.parametrize(
    ("path", "home", "expected"),
    [
        ("/home/u/proj/a.txt", "/home/u", "proj/a.txt"),
        ("/home/u", "/home/u", ""),
        ("/home/user2/x", "/home/u", None),
        ("/home/u/../v/x", "/home/u", None),
        ("relative/x", "/home/u", None),
        ("C:\\Users\\Ann\\proj\\x.py", "C:\\Users\\ann", "proj/x.py"),
        ("c:/users/ann/x", "C:\\Users\\Ann", "x"),
        ("D:\\Users\\Ann\\x", "C:\\Users\\Ann", None),
    ],
)
def test_home_relative(path, home, expected):
    assert paths.home_relative(path, home) == expected


def test_fold_key_case_and_nfc():
    nfd = "Cafe\u0301/README"
    nfc = "Caf\u00e9/README"
    assert paths.fold_key(nfd, MAC) == paths.fold_key(nfc.lower(), MAC)
    assert paths.fold_key(nfd, LINUX) == nfd != nfc
    assert paths.fold_key("ABC", LINUX) == "ABC"
    assert paths.fold_key("Straße", MAC) == paths.fold_key("STRASSE", MAC)


def test_collisions():
    rels = ["Readme.md", "README.md", "src/a.py", "Cafe\u0301", "Caf\u00e9", "src/a.py"]
    assert paths.collisions(rels, MAC) == [
        ("Readme.md", "README.md"),
        ("Cafe\u0301", "Caf\u00e9"),
        ("src/a.py", "src/a.py"),
    ]
    assert paths.collisions(rels, LINUX) == [("src/a.py", "src/a.py")]


def test_strip_hfs_ignorable():
    assert paths.strip_hfs_ignorable(".g\u200cit\u200d\u202e\ufeff") == ".git"
    assert paths.strip_hfs_ignorable("plain") == "plain"


@pytest.mark.parametrize(
    ("given", "expected"),
    [
        ("/c/Users/x", "C:\\Users\\x"),
        ("/cygdrive/d/a/b", "D:\\a\\b"),
        ("/mnt/c/x", "C:\\x"),
        ("/c", "C:\\"),
        ("/home/x", "/home/x"),
        ("/mnt/data/x", "/mnt/data/x"),
        ("C:\\already", "C:\\already"),
        ("", ""),
    ],
)
def test_msys_to_windows(given, expected):
    assert paths.msys_to_windows(given) == expected


def test_route_path_allowlist():
    """``/v1/<route>`` from the router parameter; escapes, spaces, ``;`` and dot segments rejected [SEC 1.4]."""
    assert paths.route_path("jobs/01ABC/blocks") == "/v1/jobs/01ABC/blocks"
    assert paths.route_path("hello") == "/v1/hello"
    for bad in ("%2e%2e/x", "jobs/../x", "jobs/./x", "jobs//x", "/jobs", "jobs/", "a b", "a;b", "", "a?x=1", "é"):
        with pytest.raises(BadRequest):
            paths.route_path(bad)
    with pytest.raises(BadRequest):
        paths.route_path("a" * 2000)


@pytest.mark.parametrize(
    ("url", "expected"),
    [
        ("https://user:tok@github.com/o/r.git", "https://github.com/o/r.git"),
        ("https://ghp_x@github.com/o/r", "https://github.com/o/r"),
        ("http://a@b@host:8080/p?q#f", "http://host:8080/p?q#f"),
        ("ssh://git:pw@host/r.git", "ssh://git@host/r.git"),
        ("ssh://git@host/r.git", "ssh://git@host/r.git"),
        ("git@github.com:o/r.git", "git@github.com:o/r.git"),
        ("https://github.com/o/r", "https://github.com/o/r"),
        ("/local/path", "/local/path"),
    ],
)
def test_strip_url_userinfo(url, expected):
    assert paths.strip_url_userinfo(url) == expected


def test_rules_for():
    assert paths.rules_for("Windows", False) == paths.PlatformRules(windows=True, case_insensitive=True)
    assert paths.rules_for("darwin", True) == paths.PlatformRules(windows=False, case_insensitive=True)
    assert paths.rules_for("linux", False, long_paths=True).long_paths
