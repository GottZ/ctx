"""Review finding R3-M2 (the third review §3, L9 rest): the walk that learns the identities of the
credential files has a bound (``policy.LINKED_SCAN_MAX`` entries). In the reviewed version the large stores came first
and a bound that ran out let every hard link it had not reached go along: with a ``~/.kube/cache`` of 5000 entries
(kubectl's discovery cache, common on an active machine) hard links to ``~/.netrc``, Hermes' ``auth.json`` and the
profile ``.env`` were allowed again.

Now the credential files at fixed places (Hermes' stores, the names at the top of the home and one folder below
``~/.config``) are looked at first, the directories breadth first (every place's top level before anything deeper,
browser profiles last), and a bound that ran out refuses instead of allowing: a file with more than one link whose
identity is not among the ones learned is ``secret`` (released only by ``--include-secret`` on exactly that path)
when every Hermes place was looked at in full, else ``hermes`` (never released, R19).

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
CACHE_ENTRIES = 5000  # the reviewer's ~/.kube/cache (REVIEW-3 §3 R3-M2)


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


def _fill(directory: Path, n: int) -> None:
    """``n`` small files below ``directory`` in a two-level layout like kubectl's caches."""
    for i in range(n):
        sub = directory / f"g{i // 250:03d}"
        if i % 250 == 0:
            sub.mkdir(parents=True, exist_ok=True)
        (sub / f"r{i:05d}.json").write_bytes(b"{}")


@pytest.fixture
def lay(tmp_path) -> Lay:
    home = tmp_path / "u"
    root = home / ".hermes"
    profile = root / "profiles" / "work"
    project = home / "proj"
    for d in (root, profile, project):
        d.mkdir(parents=True, exist_ok=True)
    return Lay(home, root, profile, project)


@pytest.fixture
def big_kube(lay) -> Path:
    cache = lay.home / ".kube" / "cache"
    _fill(cache / "discovery", CACHE_ENTRIES)
    _write(lay.home / ".kube" / "config", "apiVersion: v1\n")
    return cache


def test_r4_ke_a_large_kube_cache_does_not_hide_the_credential_files(lay, big_kube):
    """★ R3-M2, the reviewer's case: with a ``~/.kube/cache`` of 5000 entries, hard links to ``~/.netrc`` (``secret``),
    Hermes' ``auth.json`` of the active profile, the profile ``.env`` and the root's ``.env`` (``hermes``) are refused.
    Before the fix: ``Verdict(True, None)`` for all four — the cache had used the bound up."""
    netrc = _write(lay.home / ".netrc", "machine x login y password z\n")
    auth = _write(lay.profile / "auth.json", '{"token": "abc"}\n')
    env = _write(lay.profile / ".env", "OPENAI_API_KEY=sk-test\n")
    top = _write(lay.root / ".env", "X=1\n")
    assert lay.check(_link(netrc, lay.project / "a.txt")) == SECRET
    assert lay.check(_link(auth, lay.project / "b.json")) == HERMES
    assert lay.check(_link(env, lay.project / "c.txt")) == HERMES
    assert lay.check(_link(top, lay.project / "d.txt")) == HERMES


def test_r4_ke_a_large_store_does_not_hide_a_shallow_credential_in_a_later_store(lay, big_kube):
    """★ Breadth first, Hermes' places before the home's: the top level of every credential directory is read before
    anything deeper, so with a Hermes store directory (``mcp-tokens/``) and a credential file directly in a store
    after ``.kube`` (``~/.docker/config.json``, ``~/.config/gh/hosts.yml``) the bound runs out only in the depth of
    ``.kube/cache`` — every Hermes place was read, the home credentials are ``secret`` and ``--include-secret`` on the
    workspace name releases one. Before the fix: allowed (mutant depth first: the cut leaves the Hermes place unread
    and every link becomes ``hermes``, never releasable)."""
    _write(lay.profile / "mcp-tokens" / "github.json", '{"token": "x"}\n')
    docker = _write(lay.home / ".docker" / "config.json", '{"auths": {}}\n')
    gh = _write(lay.home / ".config" / "gh" / "hosts.yml", "github.com:\n  oauth_token: x\n")
    netrc = _write(lay.home / ".netrc", "machine x login y password z\n")
    a = _link(docker, lay.project / "docker.json")
    b = _link(gh, lay.project / "hosts.txt")
    c = _link(netrc, lay.project / "netrc.txt")
    assert lay.check(a) == SECRET
    assert lay.check(b) == SECRET
    assert lay.check(c) == SECRET
    assert lay.check(a, lay.cfg(include_secret=(str(a),))) == OK
    assert lay.check(c, lay.cfg(include_secret=(str(c),))) == OK


def test_r4_ke_a_flat_store_does_not_starve_the_names_and_the_hermes_places(lay):
    """★ Order within a level: a store whose own top level is large (``~/.password-store`` with 5000 entries side by
    side) is listed after the names at the top of the home and after Hermes' places, so ``~/.netrc`` is known, every
    Hermes place was read and the link stays releasable (``secret``). Before the fix: allowed (mutant stores first:
    the cut leaves the names and the Hermes place unread — ``hermes``, never releasable)."""
    store = lay.home / ".password-store"
    store.mkdir()
    for i in range(CACHE_ENTRIES):
        (store / f"site{i:05d}.gpg").write_bytes(b"x")
    _write(lay.profile / "mcp-tokens" / "github.json", '{"token": "x"}\n')
    netrc = _write(lay.home / ".netrc", "machine x login y password z\n")
    alias = _link(netrc, lay.project / "netrc.txt")
    assert lay.check(alias) == SECRET
    assert lay.check(alias, lay.cfg(include_secret=(str(alias),))) == OK


def test_r4_ke_exhausted_bound_refuses_an_unknown_linked_file(lay, big_kube):
    """★ Fail-closed: the bound ran out in a home store, so a file with more than one link whose other name is not
    known (here a pnpm-like store outside every credential place) is refused as ``secret`` — ``--include-secret`` on
    exactly that path releases it, a file with one name is not affected. Before the fix: allowed."""
    store = _write(lay.home / ".local" / "share" / "pnpm" / "store" / "v3" / "ab" / "cdef", "module.exports=1\n")
    alias = _link(store, lay.project / "vendor" / "index.js")
    single = _write(lay.project / "plain.txt")
    assert lay.check(alias) == SECRET
    assert lay.check(alias, lay.cfg(include_secret=(str(alias),))) == OK
    assert lay.check(single) == OK


def test_r4_ke_exhausted_bound_in_a_hermes_place_is_never_released(lay):
    """★ R19: when the bound runs out before every Hermes credential place was looked at, an unknown linked file may
    be a Hermes credential — refused as ``hermes``, not released by ``--include-secret`` either. Before the fix:
    allowed."""
    _fill(lay.profile / "home" / ".cache", CACHE_ENTRIES)  # the profile's subprocess HOME (HERMES_CREDENTIAL_PATHS)
    alias = _link(_write(lay.home / "data" / "blob.json", '{"t": 1}\n'), lay.project / "t.json")
    assert lay.check(alias) == HERMES
    assert lay.check(alias, lay.cfg(include_secret=(str(alias),))) == HERMES


def test_r4_ke_complete_walk_keeps_ordinary_links_allowed(lay):
    """No false alarm while the bound holds: an ordinary hard link (pnpm store) stays allowed, a credential link is
    refused — the walk saw everything, so nothing is refused for lack of knowledge."""
    _fill(lay.home / ".kube" / "cache", 200)
    store = _write(lay.home / ".local" / "share" / "pnpm" / "store" / "ab", "x\n")
    creds = _write(lay.home / ".aws" / "credentials", "[default]\naws_secret_access_key = abc\n")
    assert lay.check(_link(store, lay.project / "index.js")) == OK
    assert lay.check(_link(creds, lay.project / "notes.txt")) == SECRET


def test_r4_ke_the_bound_still_bounds_the_walk(lay, big_kube, monkeypatch):
    """The walk looks at no more than ``LINKED_SCAN_MAX`` directory entries per scan, however large the places are
    (counted at ``os.scandir``'s entries and the stat calls of fixed places)."""
    seen = []
    real_scandir = os.scandir

    class _Counting:
        def __init__(self, it):
            self._it = it

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            self._it.close()
            return False

        def __iter__(self):
            for entry in self._it:
                seen.append(entry.path)
                yield entry

    _fill(lay.home / ".mozilla" / "firefox", 3000)
    _fill(lay.profile / "browser-profile", 3000)
    alias = _link(_write(lay.home / "store.bin", "x\n"), lay.project / "x.bin")
    monkeypatch.setattr(policy.os, "scandir", lambda p=".": _Counting(real_scandir(p)))
    assert lay.check(alias).allowed is False
    assert len(seen) <= policy.LINKED_SCAN_MAX + 1  # plus the one entry that found the bound spent


def test_r4_ke_scan_cache_gives_the_same_verdicts_when_cut(lay, big_kube):
    """One ``SourceCheckCache`` per scan gives the verdicts of uncached calls also when the walk was cut."""
    netrc = _write(lay.home / ".netrc", "machine x\n")
    store = _write(lay.home / "shared.txt", "x\n")
    paths = [_link(netrc, lay.project / "a.txt"), _link(store, lay.project / "b.txt"), _write(lay.project / "c")]
    want = [lay.check(p) for p in paths]
    assert want == [SECRET, SECRET, OK]
    cache = SourceCheckCache()
    cfg = lay.cfg()
    assert [lay.check(p, cfg, cache) for p in reversed(paths)] == list(reversed(want))


def test_r4_ke_check_extra_refuses_an_unknown_linked_file_when_cut(lay, big_kube):
    """An extra file named in the transcript goes through the same rule (``check_extra``): refused, released only by
    ``--include-secret`` on its path."""
    store = _write(lay.home / "data" / "orig.txt", "x\n")
    alias = _link(store, lay.home / "notes" / "todo.txt")
    assert policy.check_extra(alias, lay.cfg(), explicit=False, redact=lambda s: s) == SECRET
    released = lay.cfg(include_secret=(str(alias),))
    assert policy.check_extra(alias, released, explicit=False, redact=lambda s: s) == OK
