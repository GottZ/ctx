"""Review round 3, M22 rest: the filter names a received ``.gitattributes`` selects are read from the whole file —
git reads attribute files up to 100 MiB, the scan read only the first MiB, so a name behind it escaped
``_peer_filters`` and the user's global driver ran in the resumed state check (REVIEW-2 §5 M22). The blob is
streamed, never held whole in memory (DESIGN §9.2 received, SEC 3.1)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import gitapply, gitops
from ctx_handover.engine.model import DestMode
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.E._support import apply_fresh, marker_script, pack, write_global_gitconfig

CHUNK = 65536  # what ``Git.iter_z`` reads at once
PAD_LINE = b"# " + b"p" * 77 + b"\n"  # 80 bytes, a comment


def _padding(nbytes: int) -> bytes:
    return PAD_LINE * (nbytes // len(PAD_LINE))


def _names(git, user_home, attributes: bytes, *, extra: dict[str, bytes] | None = None) -> list[str]:
    r = RepoBuilder.init(user_home / "r")
    r.write(".gitattributes", attributes)
    for rel, data in (extra or {}).items():
        r.write(rel, data)
    r.write("a.txt", "a\n")
    r.commit_all("c1")
    tree = r.out("rev-parse", "HEAD^{tree}")
    return gitops.attribute_filter_names(git, r.root / ".git", tree)


def test_r3_e_m22_names_behind_the_first_mib(git, user_home):
    """★ M22 rest (probe 5 of the reviewer): a 1.1 MiB ``.gitattributes`` naming ``peerchosen`` only after its
    first MiB — the name is found, like the one of a small nested file (mutant: only the first MiB scanned)."""
    big = _padding(1_111_024) + b"*.txt filter=peerchosen\n"
    assert len(big) > 1024 * 1024
    names = _names(git, user_home, big, extra={"sub/.gitattributes": b"*.bin filter=small\n"})
    assert names == ["peerchosen", "small"]


@pytest.mark.parametrize("split", [0, 3, 7, 10, 15])
def test_r3_e_m22_name_across_a_read_boundary(git, user_home, split):
    """★ A token cut by the 64 KiB read boundary (inside ``filter=``, between ``=`` and the name, inside the name, at
    its end) is found whole — and no cut-off part of it is taken for a name (mutant: chunks scanned without the
    carried rest)."""
    token = b"filter=straddle"
    head = _padding(CHUNK - 400)
    head += b"x" * (CHUNK - split - len(head) - 3) + b"\n* "
    assert len(head) == CHUNK - split
    names = _names(git, user_home, head + token + b"\n" + _padding(CHUNK))
    assert names == ["straddle"]


def test_r3_e_m22_over_long_line(git, user_home):
    """A line longer than a read (no newline within 64 KiB): names in it and right after it are found whole, a word
    that merely ends in ``filter=`` is not taken for an attribute (git ≥ 2.38.3 ignores such lines; a scan that
    names more only empties more drivers)."""
    long_line = b"*.a" + b" -x" * 60_000 + b" filter=deep" + b" -y" * 30_000 + b" notfilter=fake\n"
    names = _names(git, user_home, long_line + b"*.b filter=after\n")
    assert names == ["after", "deep"]


def test_r3_e_m22_over_long_line_read_boundaries(git, user_home):
    """The tail of an over-long line goes on with the next read: a name cut at the read boundary (``deep|name``) is
    taken whole, never its first half, and a word ending in ``filter=`` that the tail happens to start inside
    (``x|filter=bad``) is no attribute (mutants: the cut token accepted; the tail scanned without its context)."""
    line = bytearray(b"a" * 140_000)
    line[130_815 : 130_815 + 12] = b"xfilter=bad "  # the tail of the second read starts at 130 816: "filter=bad"
    line[131_060 : 131_060 + 17] = b" filter=deepname "  # "deep" ends at 131 072, the end of the second read
    names = _names(git, user_home, bytes(line) + b"\n*.b filter=after\n")
    assert names == ["after", "deepname"]


def test_r3_e_m22_resume_runs_no_driver_named_behind_the_first_mib(
    git, user_home, tmp_path, staging, cfg, store, handover_root, monkeypatch
):
    """★ M22 rest at the production path: an update whose received ``.gitattributes`` names ``peerchosen`` only after
    1.1 MiB — ``commit`` and the resumed ``commit`` run the target user's global ``peerchosen`` driver nowhere
    (mutant: only the first MiB scanned ⇒ the ``global-clean`` marker of the resume, as in the reviewer's probe)."""
    markers = tmp_path / "markers"
    markers.mkdir()
    clean = marker_script(tmp_path / "clean.sh", markers, "global-clean")
    (tmp_path / "peer-home").mkdir()
    peer_git = gitops.Git(home=tmp_path / "peer-home")
    peer = RepoBuilder.init(user_home / "peer")
    peer.write("a.txt", "a\n")
    peer.commit_all("c1")
    _, seed = pack(peer_git, peer.root, staging, cfg, store, item_id="seed")
    dest = apply_fresh(git, store, seed, handover_root / "app", cfg)
    tgt = RepoBuilder(dest)
    tgt.run("reset", "-q", "--hard")
    tgt.run("clean", "-qfdx")
    attributes = _padding(1_111_024) + b"*.txt filter=peerchosen\n"
    peer.write(".gitattributes", attributes)
    peer.commit_all("attributes")
    peer.write("b.txt", "peer file\n")
    write_global_gitconfig(user_home, f'[filter "peerchosen"]\n\tclean = {clean}\n\trequired = true\n')
    basis = gitops.GitProbe(git).ref_tips(str(dest))
    _, item = pack(peer_git, peer.root, staging, cfg, store, basis=basis, item_id="u")
    state = gitapply.prepare_update(git, store, item, dest, cfg=cfg, qdir=staging / "q")
    assert state.mode is DestMode.UPDATE
    gitapply.commit(git, state, item)
    assert sorted(p.name for p in markers.iterdir()) == []
    gitapply.commit(git, state, item, resume=True)
    assert sorted(p.name for p in markers.iterdir()) == []
    assert (dest / "b.txt").read_text() == "peer file\n"
    assert (dest / ".gitattributes").read_bytes() == attributes
