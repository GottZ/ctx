"""Review round rv-e2, wish W5 (i-f20): the user's global git configuration follows ``HOME`` — the process home, or
the home a ``Git`` is pinned to (``Git(home=…)``, the home Hermes gives the agent's terminal). Whichever home it
is, its dangerous values (``core.fsmonitor``, ``core.hooksPath``, ``credential.helper``, filter drivers) act on no
git call of the plugin (DESIGN §9.2, SEC 3.1/3.3), and the received profile reads nothing from that home but the
safelist — the XDG default ignore file included."""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import apply_fresh, marker_script, pack

HOOKS = ("reference-transaction", "post-index-change", "post-checkout", "pre-commit", "post-commit")


def _dangerous_config(home: Path, markers: Path, tag: str, *, layout: str) -> Path:
    """A global config whose every value runs a marker script when git honours it, where git reads it for the
    home ``home``: ``~/.gitconfig`` (``home``), ``$XDG_CONFIG_HOME/git/config`` as the process sets it (``xdg``),
    ``~/.config/git/config`` with ``XDG_CONFIG_HOME`` unset (``xdg-default``)."""
    hooks = home / f"hooks-{tag}"
    hooks.mkdir(parents=True, exist_ok=True)
    for name in HOOKS:
        marker_script(hooks / name, markers, f"{tag}-hook-{name}")
    fsmon = marker_script(home / f"fsmonitor-{tag}.sh", markers, f"{tag}-fsmonitor")
    helper = marker_script(home / f"credential-{tag}.sh", markers, f"{tag}-credential")
    clean = marker_script(home / f"clean-{tag}.sh", markers, f"{tag}-filter-clean")
    smudge = marker_script(home / f"smudge-{tag}.sh", markers, f"{tag}-filter-smudge")
    text = (
        f"[core]\n\tfsmonitor = {fsmon}\n\thooksPath = {hooks}\n"
        f"[credential]\n\thelper = {helper}\n"
        f'[filter "evil"]\n\tclean = {clean}\n\tsmudge = {smudge}\n\trequired = true\n'
    )
    if layout == "home":
        path = home / ".gitconfig"
    elif layout == "xdg":
        path = Path(os.environ["XDG_CONFIG_HOME"]) / "git" / "config"
    else:
        assert "XDG_CONFIG_HOME" not in os.environ
        path = home / ".config" / "git" / "config"
    path.parent.mkdir(parents=True, exist_ok=True)
    assert "[user]" not in text and "email" not in text.lower()
    path.write_text(text, encoding="utf-8")
    return path


def _repo_selecting_evil(root: Path) -> RepoBuilder:
    """A repository whose ``.gitattributes`` selects the ``evil`` driver (built before any dangerous config)."""
    r = RepoBuilder.init(root)
    r.write(".gitattributes", "*.txt filter=evil\n")
    r.write("a.txt", "a\n")
    r.write("plain.md", "p\n")
    r.commit_all("c1")
    return r


def _calls(git: gitops.Git, repo: Path, spec: gitops.GitEnvSpec, env_extra: dict | None = None) -> None:
    """git calls that consult each dangerous value: status (fsmonitor), add/update-index (post-index-change,
    clean filter), update-ref (reference-transaction), checkout-index (smudge filter), credential fill (helper)."""
    (repo / "a.txt").write_text("a changed\n")
    (repo / "b.txt").write_text("b new\n")
    git.run(["status", "--porcelain=v2", "-z", "-uall"], cwd=repo, spec=spec, env_extra=env_extra, check=False)
    git.run(["add", "-A"], cwd=repo, spec=spec, env_extra=env_extra, check=False)
    git.run(["update-ref", "refs/heads/probe", "HEAD"], cwd=repo, spec=spec, env_extra=env_extra, check=False)
    os.unlink(repo / "a.txt")
    git.run(["checkout-index", "-f", "--", "a.txt"], cwd=repo, spec=spec, env_extra=env_extra, check=False)
    git.run(
        ["credential", "fill"],
        cwd=repo,
        spec=spec,
        env_extra=env_extra,
        input=b"protocol=https\nhost=example.invalid\n\n",
        check=False,
    )


@pytest.fixture
def other_home(tmp_path) -> Path:
    """A second home — the process home and the pinned home differ (``plugins.isolation: host`` vs in-process)."""
    home = tmp_path / "other-home"
    home.mkdir()
    return home


@pytest.mark.parametrize("layout", ["home", "xdg", "xdg-default"])
@pytest.mark.parametrize("where", ["process", "pinned"])
@pytest.mark.parametrize("profile", ["source", "received"])
def test_rv_e2_w5_dangerous_global_values_never_act(
    user_home, other_home, tmp_path, monkeypatch, profile, where, layout
):
    """★ W5: fsmonitor, hooksPath, credential helper and filter drivers of the global config in the home git reads
    (the process home; or the pinned home of ``Git(home=…)``) run on no call of the plugin's runner. Positive
    control first: the same calls without the forced options run every marker (mutants: forced options dropped;
    received profile keeping the global config). Filters in the source profile are DESIGN behaviour (the user's own
    attributes select the user's own drivers) and are covered by the home-follow test below. A pinned home sets
    ``HOME`` only — an inherited ``XDG_CONFIG_HOME`` keeps applying, as for the agent's terminal (``xdg``)."""
    if layout == "xdg-default":
        monkeypatch.delenv("XDG_CONFIG_HOME")
    markers = tmp_path / "markers"
    markers.mkdir()
    home = user_home if where == "process" else other_home
    git = gitops.Git() if where == "process" else gitops.Git(home=other_home)
    repo = _repo_selecting_evil(user_home / "repo")
    _dangerous_config(home, markers, "g", layout=layout)
    spec = gitops.GitEnvSpec(profile=profile, ceiling=repo.root.parent)

    _calls(git, repo.root, gitops.GitEnvSpec(profile="source", ceiling=repo.root.parent), {"GIT_CONFIG_COUNT": "0"})
    fired = sorted(p.name for p in markers.iterdir())
    for want in ("g-fsmonitor", "g-hook-post-index-change", "g-hook-reference-transaction", "g-credential"):
        assert want in fired, f"control: {want} did not fire ({fired})"
    assert "g-filter-clean" in fired or "g-filter-smudge" in fired, fired
    for p in markers.iterdir():
        p.unlink()
    repo.run("reset", "-q", "--hard")
    repo.run("update-ref", "-d", "refs/heads/probe")

    if profile == "source":
        repo.write(".gitattributes", "")  # the user's own attributes select no driver of the global config
        repo.commit_all("no filter")
    _calls(git, repo.root, spec)
    assert sorted(p.name for p in markers.iterdir()) == []


@pytest.mark.parametrize("where", ["process", "pinned"])
def test_rv_e2_w5_full_flow_target_ignores_global_values(
    user_home, other_home, tmp_path, staging, cfg, store, handover_root, where
):
    """★ W5 over the plugin's own flow: a received tree selecting ``evil``; the target home (process home, or the
    pinned home) defines ``evil`` and every other dangerous value. Fresh apply, the target probes, update apply and
    the state check run no marker; with a pinned home every git process of the target carries that home (mutants:
    the pinned home ignored ⇒ HOME assertion fails; forced options dropped ⇒ markers)."""
    markers = tmp_path / "markers"
    markers.mkdir()
    (tmp_path / "source-home").mkdir()
    source_git = gitops.Git(home=tmp_path / "source-home")
    src = _repo_selecting_evil(user_home / "src")
    src.write("b.txt", "untracked\n")
    _, item = pack(source_git, src.root, staging, cfg, store, item_id="a")

    target_home = user_home if where == "process" else other_home
    git = gitops.Git() if where == "process" else gitops.Git(home=other_home)
    _dangerous_config(target_home, markers, "t", layout="home")
    seen: list[str | None] = []
    real = subprocess.Popen

    def spy(argv, *a, **kw):
        if argv and str(argv[0]) == git.exe:
            seen.append((kw.get("env") or {}).get("HOME"))
        return real(argv, *a, **kw)

    probe = gitops.GitProbe(git, profile="received")
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(gitops.subprocess, "Popen", spy)
        dest = apply_fresh(git, store, item, handover_root / "app", cfg)
        probe.info(str(dest), cfg)
    tgt = RepoBuilder(dest)
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    src.write("c.txt", "third\n")
    _, item2 = pack(source_git, src.root, staging, cfg, store, basis=probe.ref_tips(str(dest)), item_id="b")
    with pytest.MonkeyPatch.context() as mp:
        mp.setattr(gitops.subprocess, "Popen", spy)
        assert probe.status_clean(str(dest))[0]
        state = gitapply.prepare_update(git, store, item2, dest, cfg=cfg, qdir=staging / "q")
        gitapply.commit(git, state, item2)
        gitops.worktree_state(git, str(dest), (), tmp_path / "state", profile="received")
    assert (dest / "c.txt").read_bytes() == b"third\n"
    assert sorted(p.name for p in markers.iterdir()) == []
    assert len(seen) > 20
    assert set(seen) == {str(target_home)}


def test_rv_e2_w5_source_reads_the_pinned_home(user_home, other_home, tmp_path, staging, cfg, store):
    """★ W5 home-follow: with ``Git(home=H)`` the source profile reads H's global config — its filter driver cleans
    the user's own ``*.txt`` (DESIGN §9.2 source: same objects as the user's own ``git add``) and the process home's
    driver of the same name never runs; the received safelist comes from H too, and a ``Git()`` without a pinned
    home in the same process keeps its own (mutants: ``home`` ignored; safelist cache keyed by executable only)."""
    markers = tmp_path / "markers"
    markers.mkdir()
    pinned_clean = marker_script(tmp_path / "pinned.sh", markers, "pinned-clean")
    process_clean = marker_script(tmp_path / "process.sh", markers, "process-clean")
    (other_home / ".gitconfig").write_text(
        f'[filter "probe"]\n\tclean = {pinned_clean}\n[core]\n\tautocrlf = input\n', encoding="utf-8"
    )
    (user_home / ".gitconfig").write_text(
        f'[filter "probe"]\n\tclean = {process_clean}\n[core]\n\tautocrlf = true\n', encoding="utf-8"
    )
    src = RepoBuilder.init(user_home / "src")
    src.write(".gitattributes", "*.txt filter=probe\n")
    src.commit_all("attrs")
    src.write("new.txt", "new\n")
    pinned = gitops.Git(home=other_home)
    pack(pinned, src.root, staging, cfg, store)
    assert sorted(p.name for p in markers.iterdir()) == ["pinned-clean"]

    def autocrlf(g: gitops.Git) -> list[str]:
        env = g.env(gitops.RECEIVED)
        n = int(env["GIT_CONFIG_COUNT"])
        return [env[f"GIT_CONFIG_VALUE_{i}"] for i in range(n) if env[f"GIT_CONFIG_KEY_{i}"] == "core.autocrlf"]

    assert autocrlf(pinned) == ["input"]
    assert autocrlf(gitops.Git()) == ["true"]
    assert autocrlf(pinned) == ["input"]


def test_rv_e2_w5_pinned_home_must_be_absolute():
    with pytest.raises(ValueError):
        gitops.Git(home="relative/home")


@pytest.mark.parametrize("xdg", ["set", "unset"])
def test_rv_e2_w5_received_ignores_global_ignore_file(user_home, tmp_path, monkeypatch, xdg):
    """★ W5: the XDG default ignore file (``$XDG_CONFIG_HOME/git/ignore``, else ``~/.config/git/ignore``) follows
    HOME like ``~/.gitconfig`` and is read even with the global config nulled. A received-profile call reads neither
    (mutant: received profile without ``core.excludesFile=/dev/null``). The update precondition (``status_clean``)
    must not depend on where the user keeps a global ignore file: since review round 3 (R2-M4) it reads that file
    in both places alike, as the user's own ``git status`` does — the protection of what lies in the way is
    ``gitapply.obstructions`` (B2), not the clean status."""
    if xdg == "unset":
        monkeypatch.delenv("XDG_CONFIG_HOME", raising=False)
    ignore = user_home / ".config" / "git" / "ignore"
    ignore.parent.mkdir(parents=True)
    ignore.write_text("*.junk\n", encoding="utf-8")
    (user_home / "explicit-ignore").write_text("*.other\n", encoding="utf-8")
    git = gitops.Git()
    r = RepoBuilder.init(user_home / "repo")
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    probe = gitops.GitProbe(git, profile="received")
    received = gitops.GitEnvSpec(profile="received", ceiling=r.root.parent)

    def untracked() -> list[str]:
        return git.out(["ls-files", "--others", "--exclude-standard"], cwd=r.root, spec=received).split("\n")

    assert probe.status_clean(str(r.root)) == (True, "")
    r.write("x.junk", "j\n")
    assert "x.junk" in untracked()
    assert probe.status_clean(str(r.root)) == (True, "")
    r.write("plain.txt", "p\n")
    clean, why = probe.status_clean(str(r.root))
    assert clean is False and "plain.txt" in why
    r.remove("plain.txt")
    r.remove("x.junk")
    (user_home / ".gitconfig").write_text(f"[core]\n\texcludesFile = {user_home / 'explicit-ignore'}\n")
    gitops.reset_process_caches()
    r.write("y.other", "o\n")
    assert "y.other" in untracked()
    assert probe.status_clean(str(r.root)) == (True, "")
    source = gitops.GitProbe(git)
    assert source.is_ignored(str(r.root)) is False
    assert git.out(["check-ignore", "y.other"], cwd=r.root, spec=gitops.GitEnvSpec(profile="source")) == "y.other"
