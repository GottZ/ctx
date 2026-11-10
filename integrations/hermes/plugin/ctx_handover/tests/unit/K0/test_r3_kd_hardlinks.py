"""Review finding L9 (the second review §4, hardening checklist BUILD-ORDER §6 no. 17): a hard link
to a credential file goes along silently.

A hard link is a second name of the same file: no symlink to resolve, no denied name, and a credential file without a
private-key marker (``~/.aws/credentials``, ``~/.netrc``, the Hermes ``.env``) passes the content sniff. The policy
now compares the identity ``(st_dev, st_ino)`` of every regular file with more than one link against the identities
of the credential files it knows — the secrets directories and denied names in ``$HOME``, the Hermes credential
stores of the active home, the root and every profile — and treats a match like a symlink to that file: ``secret``
(liftable by ``--include-secret`` on the name in the workspace) or ``hermes`` (never liftable). A hard link between
two ordinary files (pnpm's store, ``cp -l`` backups) stays allowed.

Real files in tmp, the policy built as production builds it (``PolicyConfig.from_options``).
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig, SourceCheckCache, Verdict

OK = Verdict(True, None)
SECRET = Verdict(False, "secret")
HERMES = Verdict(False, "hermes")
AWS = "[default]\naws_access_key_id = AKIAIOSFODNN7EXAMPLE\naws_secret_access_key = wJalrXUtnFEMI/K7MDENG\n"


@dataclass
class Lay:
    home: Path
    root: Path
    profile: Path
    project: Path

    def cfg(self, include_secret: tuple[str, ...] = ()) -> PolicyConfig:
        return PolicyConfig.from_options(
            Options(),
            home=self.home,
            hermes_home=self.profile,
            hermes_root=self.root,
            plugin_dir=self.profile / "plugin-data" / "ctx_handover",
            case_insensitive=False,
            target_os="linux",
            include_secret=include_secret,
        )

    def check(self, path: Path, cfg: PolicyConfig | None = None, cache: SourceCheckCache | None = None) -> Verdict:
        return policy.check_source_file(path, cfg or self.cfg(), explicit=False, item_root=self.project, cache=cache)


def _write(path: Path, text: str = "hello\n") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


def _link(target: Path, alias: Path) -> Path:
    alias.parent.mkdir(parents=True, exist_ok=True)
    os.link(target, alias)
    assert os.stat(alias).st_nlink >= 2
    return alias


@pytest.fixture
def lay(tmp_path) -> Lay:
    home = tmp_path / "u"
    root = home / ".hermes"
    profile = root / "profiles" / "work"
    project = home / "proj"
    for d in (root, profile, project):
        d.mkdir(parents=True, exist_ok=True)
    return Lay(home, root, profile, project)


def test_l9_hardlink_to_aws_credentials_is_refused(lay):
    """★ The reviewer's case: ``proj/notes.txt`` is a hard link to ``~/.aws/credentials`` (no key marker). Before the
    fix: ``Verdict(True, None)`` — the file went along without a word."""
    creds = _write(lay.home / ".aws" / "credentials", AWS)
    alias = _link(creds, lay.project / "notes.txt")
    assert lay.check(alias) == SECRET


@pytest.mark.parametrize(
    "rel",
    [
        ".netrc",
        ".pgpass",
        ".git-credentials",
        ".config/gh/hosts.yml",
        ".kube/config",
        ".docker/config.json",
        ".ssh/config",
        ".config/rclone/rclone.conf",
        ".config/pip/pip.conf",
    ],
)
def test_l9_hardlink_to_a_credential_file_of_the_home_is_refused(lay, rel):
    """★ Every place the denylist names in ``$HOME`` — a secrets directory or a denied file name (top level and one
    folder below ``~/.config``) — is a known credential file. Before the fix: allowed."""
    target = _write(lay.home / rel, "token = abc\n")
    alias = _link(target, lay.project / "sub" / "plain.txt")
    assert lay.check(alias) == SECRET, rel


@pytest.mark.parametrize("rel", [".env", "auth.json", "mcp-tokens/github.json", "pairing/peer.json"])
def test_l9_hardlink_to_a_hermes_credential_is_never_released(lay, rel):
    """★ A hard link to a credential store of the active Hermes home is ``hermes`` — R19: never transferable, also not
    with ``--include-secret`` on the name in the workspace. Before the fix: allowed."""
    target = _write(lay.profile / rel, "OPENAI_API_KEY=sk-test\n")
    alias = _link(target, lay.project / "cfg.txt")
    assert lay.check(alias) == HERMES, rel
    assert lay.check(alias, lay.cfg(include_secret=(str(alias),))) == HERMES, rel


def test_l9_hardlink_to_another_profile_or_the_root_credentials_is_hermes(lay):
    """The Hermes root and every profile below it count, not only the active home."""
    other = _write(lay.root / "profiles" / "other" / "auth.json", "{}")
    top = _write(lay.root / ".env", "X=1\n")
    assert lay.check(_link(other, lay.project / "a.json")) == HERMES
    assert lay.check(_link(top, lay.project / "b.txt")) == HERMES


def test_l9_include_secret_on_the_workspace_name_lifts_a_home_credential(lay):
    """Like a symlink to a secret: ``--include-secret`` on exactly that path releases it (R19 keeps only Hermes
    credentials unliftable)."""
    creds = _write(lay.home / ".aws" / "credentials", AWS)
    alias = _link(creds, lay.project / "notes.txt")
    assert lay.check(alias, lay.cfg(include_secret=(str(alias),))) == OK


def test_l9_hardlinks_between_ordinary_files_stay_allowed(lay):
    """No false alarm: pnpm's content-addressed store and ``cp -l`` backups link ordinary files many times."""
    store = _write(
        lay.home / ".local" / "share" / "pnpm" / "store" / "v3" / "files" / "ab" / "cdef", "module.exports=1\n"
    )
    first = _link(store, lay.project / "node_modules" / ".pnpm" / "a" / "index.js")
    second = _link(store, lay.project / "vendor" / "index.js")
    assert lay.check(first) == OK
    assert lay.check(second) == OK


def test_l9_a_single_name_is_not_a_hardlink(lay):
    """``st_nlink == 1`` never reaches the identity check (the common case stays as cheap as before)."""
    _write(lay.home / ".aws" / "credentials", AWS)
    copy = _write(lay.project / "copy.txt", AWS)
    assert os.stat(copy).st_nlink == 1
    assert lay.check(copy) == OK


def test_l9_scan_cache_gives_the_same_verdicts(lay):
    """One ``SourceCheckCache`` per scan (the identities are collected once, on the first linked file) gives exactly
    the verdicts of uncached calls, in either order."""
    creds = _write(lay.home / ".aws" / "credentials", AWS)
    env = _write(lay.profile / ".env", "K=v\n")
    plain = _write(lay.home / "shared.txt", "x\n")
    paths = [
        _link(creds, lay.project / "a.txt"),
        _link(env, lay.project / "b.txt"),
        _link(plain, lay.project / "c.txt"),
        _write(lay.project / "d.txt"),
    ]
    want = [lay.check(p) for p in paths]
    assert want == [SECRET, HERMES, OK, OK]
    for order in (paths, list(reversed(paths))):
        cache = SourceCheckCache()
        cfg = lay.cfg()
        got = {p: lay.check(p, cfg, cache) for p in order}
        assert [got[p] for p in paths] == want


def test_l9_identity_walk_is_bounded(lay, monkeypatch):
    """The walk over the credential places stops after ``policy.LINKED_SCAN_MAX`` directory entries: a huge browser
    profile never makes a scan slow. The small credential stores come first, so ``.aws`` is still known. (R3-M2:
    beyond the bound the walk is incomplete and refuses — ``secret`` — instead of allowing; the bound is set
    above the fixed places, which are looked at first.)"""
    creds = _write(lay.home / ".aws" / "credentials", AWS)
    big = lay.home / ".mozilla" / "firefox" / "x.default"
    for i in range(400):
        _write(big / f"f{i:03d}.sqlite", "x")
    late = _write(big / "zzz-logins.json", "{}")
    monkeypatch.setattr(policy, "LINKED_SCAN_MAX", 300)
    assert lay.check(_link(creds, lay.project / "a.txt")) == SECRET
    # beyond the bound: not known, refused (fail-closed), never an error
    assert lay.check(_link(late, lay.project / "b.json")) == SECRET


def test_l9_check_extra_refuses_a_linked_credential(lay):
    """An extra file named in the transcript goes through the same rule (``check_extra``)."""
    creds = _write(lay.home / ".netrc", "machine x login y password z\n")
    alias = _link(creds, lay.home / "notes" / "todo.txt")
    verdict = policy.check_extra(alias, lay.cfg(), explicit=False, redact=lambda s: s)
    assert verdict == SECRET
