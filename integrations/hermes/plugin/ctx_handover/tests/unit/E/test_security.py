"""Git-related security tests of the workspace cluster (TESTPLAN §5: T3, T6, T8; SEC 3.2, 3.4)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.tests.unit.E._support import build_mixed, pack

BAD_REFS = [
    "refs/heads/../../x",
    "refs/hooks/x",
    "refs/heads/",
    "refs/heads",
    "HEAD",
    "-x",
    "refs/heads/a..b",
    "refs/heads/x\n",
    "refs/heads/x\x00y",
    "refs/heads/x.lock",
    "refs/heads/@{1}",
    "refs/heads/ x",
    "refs/ctxh/head",
    "refs/remotes/origin/../../../hooks",
    "refs//heads/x",
]
GOOD_REFS = [
    "refs/heads/main",
    "refs/heads/feat/x-1",
    "refs/tags/v1.0",
    "refs/remotes/origin/main",
    "refs/notes/commits",
]


def test_sec_ref_names_validated(git, user_home, tmp_path, staging, cfg, store):
    """★ ref names from a peer pass the namespace allowlist and ``check-ref-format``; a pack document naming a
    forbidden ref is refused in the quarantine, which is deleted (mutant: no namespace allowlist) [SEC 3.2, T6]."""
    for name in BAD_REFS:
        with pytest.raises(PolicyDenied):
            gitops.validate_ref(git, name)
    for name in GOOD_REFS:
        assert gitops.validate_ref(git, name) == name
    assert gitops.validate_ref(git, "refs/ctxh/stash/3", allow_ctxh=True) == "refs/ctxh/stash/3"
    src = build_mixed(user_home / "src")
    _, item = pack(git, src.root, staging, cfg, store)
    head = item["head"]["commit"]
    for name in ("refs/hooks/post-checkout", "refs/heads/../config"):
        bad = {**item, "refs": {**item["refs"], name: head}}
        qdir = tmp_path / "q"
        with pytest.raises(PolicyDenied):
            gitapply.quarantine_and_verify(git, store, bad, qdir, alternates=None)
        assert not qdir.exists()
    with pytest.raises(PolicyDenied):
        gitapply.parse_item({**item, "head": {"ref": "refs/tags/v1", "commit": head}})


BAD_URLS = [
    "ext::sh -c touch% /tmp/pwned",
    "fd::17",
    "file:///etc",
    "file://localhost/srv/repo.git",
    "-uconfig",
    "--upload-pack=touch /tmp/x",
    "https://example.org/r.git\r\nfetch = x",
    "https://example.org/r.git\n",
    "https://example.org/\x00r.git",
    "/srv/repo.git",
    "../sibling.git",
    "./here",
    "C:\\repos\\x",
    "c:/repos/x",
    "ssh://-oProxyCommand=touch%20x/r.git",
    "-host:path",
    "transport::https://example.org/r.git",
    "ftp://example.org/r.git",
    "",
]


def test_sec_remote_url_and_ref_validation(git, user_home, tmp_path, staging, cfg, store):
    """★ remote names/URLs from a peer: helper transports, ``file:``, local paths, a leading ``-``, CR/LF/NUL are
    refused; credentials are stripped; only the five config keys are written (mutant: no transport check) [SEC 3.4,
    CVE-2025-48384 class]."""
    for url in BAD_URLS:
        with pytest.raises(PolicyDenied):
            gitops.validate_remote("origin", url)
    for name in ("-o", ".hidden", "a b", "x" * 65, "a..b", "re\nmote", ""):
        with pytest.raises(PolicyDenied):
            gitops.validate_remote(name, "https://example.org/r.git")
    assert gitops.validate_remote("origin", "https://u:tok@example.org/r.git") == (
        "origin",
        "https://example.org/r.git",
    )
    assert gitops.validate_remote("up", "ssh://git:pw@example.org:22/r.git") == ("up", "ssh://git@example.org:22/r.git")
    assert gitops.validate_remote("gh", "git@github.com:o/r.git") == ("gh", "git@github.com:o/r.git")
    assert gitops.validate_remote("g", "git://example.org/r.git")[1] == "git://example.org/r.git"
    from ctx_handover.tests.fakes.repo_builder import RepoBuilder

    target = RepoBuilder.init(tmp_path / "t")
    cfg_file = target.root / ".git" / "config"
    before = cfg_file.read_text()
    warnings = gitapply.write_remote_config(
        git,
        target.root,
        [
            {"name": "origin", "fetch_url": "https://u:p@example.org/r.git", "push_url": "ext::sh"},
            {"name": "evil", "fetch_url": "ext::sh -c id", "push_url": None},
            {"name": "-x", "fetch_url": "https://example.org/r.git", "push_url": None},
        ],
        {
            "main": {"remote": "origin", "merge": "refs/heads/main"},
            "bad": {"remote": "evil", "merge": "refs/heads/main"},
            "inj\n[core]": {"remote": "origin", "merge": "refs/heads/main"},
            "x": {"remote": "origin", "merge": "refs/hooks/x"},
        },
    )
    assert len(warnings) == 6
    added = cfg_file.read_text()[len(before) :]
    keys = git.out(["config", "--file", str(cfg_file), "--list"], cwd=None, spec=gitops.RECEIVED).split("\n")
    new = sorted(k for k in keys if k.split("=", 1)[0].startswith(("remote.", "branch.")))
    assert new == [
        "branch.main.merge=refs/heads/main",
        "branch.main.remote=origin",
        "remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
        "remote.origin.url=https://example.org/r.git",
    ]
    assert "ext::" not in added and "[core]" not in added
    assert cfg_file.read_text().startswith(before)


def test_sec_lfsconfig_url_not_contacted(git, user_home, tmp_path, staging, cfg, store):
    """A received ``.lfsconfig``/remote URL pointing at a listener is never contacted, neither by fresh nor by
    update mode (``protocol.allow=never``, no fetch protocol, LFS objects only from the pack) [T8]."""
    import socket

    from ctx_handover.tests.fakes.repo_builder import RepoBuilder
    from ctx_handover.tests.unit.E._support import append_repo_config, apply_fresh

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen(4)
    listener.settimeout(0.3)
    url = f"http://127.0.0.1:{listener.getsockname()[1]}/r.git"
    src = RepoBuilder.init(user_home / "src")
    src.write(".lfsconfig", f"[lfs]\n\turl = {url}/info/lfs\n")
    src.write(".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
    src.write("a.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:" + "a" * 64 + "\nsize 3\n")
    src.commit_all("c1")
    append_repo_config(src.root, f'[remote "origin"]\n\turl = {url}\n')
    _, item = pack(git, src.root, staging, cfg, store)
    dest = apply_fresh(git, store, item, user_home / "hermes-handover" / "app", cfg)
    src.write("b.txt", "b\n")
    _, item2 = pack(git, src.root, staging, cfg, store, basis=gitops.GitProbe(git).ref_tips(str(dest)), item_id="u")
    state = gitapply.prepare_update(git, store, item2, dest, cfg=cfg, qdir=staging / "q")
    gitapply.commit(git, state, item2)
    with pytest.raises(socket.timeout):
        listener.accept()
    listener.close()
