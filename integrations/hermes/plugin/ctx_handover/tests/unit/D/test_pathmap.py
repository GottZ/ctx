"""D: target path mapping (DESIGN §9.7, §10.4, R4, R19).

Rules, first match wins: (1) local ``--dest``/``--map``; (2) return mapping from this host's workspace map;
(3) the source's ``suggested_dest``; (4) same path; (5) home-relative; (6) ``<workspace_root>/<basename>``.
Rules 3–5 — and every destination the counterpart proposes — only under ``allow_roots``.
"""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path

from ctx_handover.engine import pathmap
from ctx_handover.engine.model import ItemKind, ItemRole, MapRow, Options, Origin
from ctx_handover.engine.policy import check_target_dest
from ctx_handover.tests.fakes.planning_fake import env_facts, plan_item

SRC = env_facts("server", home="/home/u")
TGT = env_facts("laptop", home="/home/u", allow_roots=("/home/u/hermes-handover", "/home/u/work"))


def _lookup(rows: dict[tuple[str, str], str]):
    def lookup(remote_path: str, direction: str):
        local = rows.get((remote_path, direction))
        return (
            None
            if local is None
            else MapRow(local_path=local, peer_id="peer-1", remote_path=remote_path, direction=direction)
        )

    return lookup


NONE = _lookup({})


def _choose(item, *, lookup=NONE, options=None, src=SRC, tgt=TGT, **kw):
    return pathmap.choose_dest(
        item, source_facts=src, target_facts=tgt, lookup=lookup, options=options or Options(), **kw
    )


def test_pathmap_rules_order():
    item = plan_item(source_path="/home/u/work/app", suggested_dest="/home/u/work/app-suggested")
    ret = _lookup({("/home/u/work/app", "out"): "/srv/back/app"})
    assert _choose(item, lookup=ret, options=Options(dest="/opt/mine")) == ("/opt/mine", "user")
    assert _choose(item, lookup=ret, options=Options(maps=(("/home/u/work", "/data/w"),))) == ("/data/w/app", "user")
    assert _choose(item, lookup=ret) == ("/srv/back/app", "return-mapping")
    assert _choose(item) == ("/home/u/work/app-suggested", "suggested")
    assert _choose(plan_item(source_path="/home/u/work/app")) == ("/home/u/work/app", "same-path")
    other_home = env_facts("laptop", home="/Users/u", allow_roots=("/Users/u/work",))
    assert _choose(plan_item(source_path="/home/u/work/app"), tgt=other_home) == ("/Users/u/work/app", "home-relative")
    assert _choose(plan_item(source_path="/srv/app")) == ("/home/u/hermes-handover/app", "workspace-root")
    # an "in" row (repeat in the same direction) maps as well
    again = _lookup({("/srv/app", "in"): "/home/u/hermes-handover/app"})
    assert _choose(plan_item(source_path="/srv/app"), lookup=again) == ("/home/u/hermes-handover/app", "return-mapping")


def test_pathmap_return_mapping_wins():
    """★ R4: the recorded return mapping beats the source's suggestion and may leave ``allow_roots``.

    Mutant: ``pathmap.choose_dest`` tries ``suggested_dest`` before the workspace-map lookup."""
    item = plan_item(source_path="/home/v/hermes-handover/app", suggested_dest="/home/u/work/elsewhere")
    lookup = _lookup({("/home/v/hermes-handover/app", "out"): "/srv/app"})
    assert _choose(item, lookup=lookup) == ("/srv/app", "return-mapping")


def test_pathmap_peer_dest_outside_roots_ignored(lay):
    """★ R19 / SEC 2.2: a destination proposed by the counterpart (``suggested_dest``, its ``--dest``/``--map``)
    is used only under ``allow_roots`` and never on the target denylist.

    Mutant: ``pathmap.choose_dest`` uses ``suggested_dest`` without the allow-root check."""
    item = plan_item(source_path="/srv/app", suggested_dest="/home/u/.config/autostart/app")
    assert _choose(item) == ("/home/u/hermes-handover/app", "workspace-root")
    item = plan_item(source_path="/srv/app", suggested_dest="/etc/cron.d/app")
    assert _choose(item) == ("/home/u/hermes-handover/app", "workspace-root")
    remote_opts = Options(dest="/home/u/.ssh/app", maps=(("/srv", "/root/x"),))
    assert _choose(plan_item(source_path="/srv/app"), options=remote_opts, local_choice=False) == (
        "/home/u/hermes-handover/app",
        "workspace-root",
    )
    ok_remote = Options(dest="/home/u/work/app2")
    assert _choose(plan_item(source_path="/srv/app"), options=ok_remote, local_choice=False) == (
        "/home/u/work/app2",
        "suggested",
    )
    # with the real policy of this host the denylist inside allow_roots holds too
    tgt = env_facts(
        "laptop", home=str(lay.home), allow_roots=(str(lay.home),), workspace_root=str(lay.home / "hermes-handover")
    )
    cfg = lay.cfg(options=Options(allow_roots=(str(lay.home),)))
    item = plan_item(source_path="/srv/app", suggested_dest=str(lay.home / ".bashrc"))
    assert _choose(item, tgt=tgt, cfg=cfg) == (str(lay.home / "hermes-handover" / "app"), "workspace-root")
    item = plan_item(source_path="/srv/app", suggested_dest=str(lay.home / "projects" / "app"))
    assert _choose(item, tgt=tgt, cfg=cfg) == (str(lay.home / "projects" / "app"), "suggested")


def test_pathmap_extras_and_hermes_files():
    extra = plan_item(
        "x0",
        "/home/u/notes/todo.md",
        kind=ItemKind.EXTRA_FILE,
        role=ItemRole.EXTRA,
        origin=Origin.TRANSCRIPT,
        suggested_dest="/home/u/work/todo.md",
    )
    dest, reason = _choose(extra, job_id="01JCABCDEFGHJKMNPQRS7XYZ12")
    assert (dest, reason) == ("/home/u/hermes-handover/_handover/7XYZ12/notes/todo.md", "workspace-root")
    ret = _lookup({("/home/u/notes/todo.md", "out"): "/home/u/todo.md"})
    assert _choose(extra, lookup=ret, job_id="01JC") == ("/home/u/todo.md", "return-mapping")
    assert _choose(extra, options=Options(maps=(("/home/u/notes", "/home/u/n2"),)), job_id="01JC") == (
        "/home/u/n2/todo.md",
        "user",
    )
    outside = plan_item("x1", "/srv/data/a.csv", kind=ItemKind.EXTRA_FILE, role=ItemRole.EXTRA)
    assert _choose(outside, job_id="01JCAAAAAABBBBBB") == (
        "/home/u/hermes-handover/_handover/BBBBBB/_abs/srv/data/a.csv",
        "workspace-root",
    )
    hermes = plan_item("h0", "/home/u/.hermes/cache/spillover/o.txt", kind=ItemKind.HERMES_FILE, role=ItemRole.HERMES)
    assert _choose(hermes) == (TGT.host.hermes_home, "hermes")


def test_pathmap_windows_target():
    win = env_facts(
        "pc",
        os_family="windows",
        home="C:\\Users\\u",
        workspace_root="C:\\Users\\u\\hermes-handover",
        allow_roots=("C:\\Users\\u\\hermes-handover",),
        case_insensitive={"C:\\Users\\u\\hermes-handover": True},
    )
    assert _choose(plan_item(source_path="/home/u/app"), tgt=win) == (
        "C:\\Users\\u\\hermes-handover\\app",
        "workspace-root",
    )
    item = plan_item(source_path="/home/u/app", suggested_dest="c:\\users\\U\\Hermes-Handover\\app")
    assert _choose(item, tgt=win) == ("c:\\users\\U\\Hermes-Handover\\app", "suggested")
    item = plan_item(source_path="/home/u/app", suggested_dest="\\\\server\\share\\app")
    assert _choose(item, tgt=win) == ("C:\\Users\\u\\hermes-handover\\app", "workspace-root")


def test_pathmap_choose_all_dedupes_basenames():
    a = plan_item("w0", "/srv/one/app")
    b = plan_item("w1", "/srv/two/app", role=ItemRole.SECONDARY)
    c = plan_item("w2", "/srv/three/app", role=ItemRole.SECONDARY)
    out = pathmap.choose_all([a, b, c], source_facts=SRC, target_facts=TGT, lookup=NONE, options=Options())
    assert [d for d, _ in out] == [
        "/home/u/hermes-handover/app",
        "/home/u/hermes-handover/app-2",
        "/home/u/hermes-handover/app-3",
    ]


def test_pathmap_fallback_dest_naming():
    now = datetime(2026, 11, 9, 14, 3, 7, tzinfo=timezone.utc)
    roots = ("/home/u/hermes-handover",)
    assert pathmap.fallback_dest(
        "/home/u/hermes-handover/app", now=now, workspace_root="/home/u/hermes-handover", allow_roots=roots
    ) == ("/home/u/hermes-handover/app-handover-20261109-140307")
    assert pathmap.fallback_dest("/srv/app", now=now, workspace_root="/home/u/hermes-handover", allow_roots=roots) == (
        "/home/u/hermes-handover/app-handover-20261109-140307"
    )
    assert (
        pathmap.fallback_dest(
            "C:\\Users\\u\\hermes-handover\\app",
            now=now,
            workspace_root="C:\\Users\\u\\hermes-handover",
            allow_roots=("C:\\Users\\u\\hermes-handover",),
        )
        == "C:\\Users\\u\\hermes-handover\\app-handover-20261109-140307"
    )


def test_map_cwd_relative_position():
    item = plan_item(source_path="/srv/app", rel_cwd="services/api")
    assert pathmap.map_cwd("/srv/app/services/api", item, "/home/u/hermes-handover/app") == (
        "/home/u/hermes-handover/app/services/api"
    )
    plain = plan_item(source_path="/srv/app")
    assert pathmap.map_cwd("/srv/app/lib", plain, "/home/v/app") == "/home/v/app/lib"
    assert pathmap.map_cwd("/elsewhere", plain, "/home/v/app") == "/home/v/app"
    hostile = plan_item(source_path="/srv/app", rel_cwd="../../.ssh")
    assert pathmap.map_cwd("/srv/app", hostile, "/home/v/app") == "/home/v/app"
    assert pathmap.map_cwd("/srv/app/x", plain, "C:\\Users\\u\\app") == "C:\\Users\\u\\app\\x"


def test_pathmap_fallback_dest_expands_the_configured_roots(lay):
    """★ V35, R19: ``Options`` carries the configured roots (``~/hermes-handover``) and ``local_target`` passes them
    as they are. The fresh directory is an expanded absolute path under the handover root that the target's real
    policy accepts (``check_target_dest`` without ``recorded``/``local_choice``, exactly as ``materialize`` checks a
    conflict's fallback) — for a first choice under the root (sibling) and outside it (under ``workspace_root``);
    the home defaults to this process's home, the home of the target's policy. With ``cfg`` a sibling the target
    denylist refuses moves under ``workspace_root``.

    Mutants (red proof, ``lab/L2-redproof/g-d``): roots used unexpanded (the Schicht-R state) ⇒ ``'~/hermes-
    handover/proj-handover-…'``; the ``cfg`` check of the sibling dropped ⇒ the fallback lies in ``~/.ssh``."""
    now = datetime(2026, 11, 9, 14, 3, 7, tzinfo=timezone.utc)
    cfg = lay.cfg()
    handover = lay.home / "hermes-handover"
    raw = {"workspace_root": Options().workspace_root, "allow_roots": Options().allow_roots}
    assert raw == {"workspace_root": "~/hermes-handover", "allow_roots": ("~/hermes-handover",)}

    inside = pathmap.fallback_dest(str(handover / "proj"), now=now, **raw)
    assert inside == str(handover / "proj-handover-20261109-140307")
    outside = pathmap.fallback_dest(str(lay.home / "proj"), now=now, **raw)
    assert outside == str(handover / "proj-handover-20261109-140307")
    assert pathmap.fallback_dest(str(handover / "proj"), now=now, **raw, cfg=cfg) == inside
    for got in (inside, outside):
        assert check_target_dest(Path(got), cfg, recorded=False, local_choice=False).allowed, got
    assert pathmap.fallback_dest("/x/home/proj", now=now, **raw, home="/x/home") == (
        "/x/home/hermes-handover/proj-handover-20261109-140307"
    )
    assert pathmap.fallback_dest("/x/home/w/proj", now=now, workspace_root="w", allow_roots=("w",), home="/x/home") == (
        "/x/home/w/proj-handover-20261109-140307"
    )

    # the whole home allowed: the sibling of a first choice in ~/.ssh is refused by the denylist (R19)
    wide = lay.cfg(options=Options(allow_roots=("~",)))
    ssh = str(lay.home / ".ssh" / "proj")
    got = pathmap.fallback_dest(ssh, now=now, workspace_root="~/hermes-handover", allow_roots=("~",), cfg=wide)
    assert got == str(handover / "proj-handover-20261109-140307")
    assert check_target_dest(Path(got), wide, recorded=False, local_choice=False).allowed
    # forms paths.expand_root refuses are no root at all (``~other/x`` is not ``<home>/~other/x``)
    odd = str(lay.home / "~other" / "x" / "proj")
    got = pathmap.fallback_dest(odd, now=now, workspace_root=str(handover), allow_roots=("~other/x", "", "//srv/s"))
    assert got == str(handover / "proj-handover-20261109-140307")
