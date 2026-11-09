"""``policy.homes_of`` (rv-d, M35): the public form of the target denylist's home list, read by the source's home rule
(``collector.HomeRule``) so both sides know the same homes."""

from __future__ import annotations

import os
from pathlib import Path

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options


def _cfg(home: Path) -> policy.PolicyConfig:
    return policy.PolicyConfig.from_options(
        Options(),
        home=home,
        hermes_home=home / ".hermes",
        hermes_root=home / ".hermes",
        plugin_dir=home / ".hermes" / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )


def test_homes_of_is_the_denylists_home_list(tmp_path):
    """★ ``homes_of`` returns exactly ``_homes_of``: the process's home as given and resolved, the superuser homes on
    POSIX, every password-database home, and ``/home/<x>`` for a path below ``/home`` (in that path's spelling) — and
    no superuser home on Windows.

    Mutant: ``homes_of`` drops the ``HOME_PARENTS`` home (``_homes_of`` called with a path outside ``/home``)."""
    real_home = tmp_path / "real-home"
    real_home.mkdir()
    link = tmp_path / "home-link"
    link.symlink_to(real_home, target_is_directory=True)
    cfg = _cfg(link)
    probe = Path("/home/alice/proj/src")
    homes = policy.homes_of(probe, cfg, windows=False)
    assert homes == policy._homes_of(probe, cfg, False)
    assert link in homes and Path(os.path.realpath(link)) in homes
    assert Path("/home/alice") in homes
    assert Path("/root") in homes
    assert Path("/home/alice") not in policy.homes_of(Path("/srv/app"), cfg, windows=False)
    assert Path("/root") not in policy.homes_of(probe, cfg, windows=True)
