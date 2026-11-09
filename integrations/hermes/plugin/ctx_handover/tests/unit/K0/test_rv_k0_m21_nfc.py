"""rv-k0, review finding M21 (``review-code/REVIEW.md`` §2, g4 M-4): the NFC part of hardening #16 had no test.

On a case-insensitive host (macOS, Windows) a path may arrive in NFD while the configured home, Hermes home and
denylists are NFC. Every comparison normalises each component to NFC (``paths._norm_parts``, ``policy._fold``,
``policy._key``). The reviewer's mutant without any NFC normalisation left K0 + D + E green (1290 passed); these
tests are red under it (red proof in the rv-k0 report).
"""

from __future__ import annotations

import unicodedata
from pathlib import Path

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig, Verdict

DENIED = Verdict(False, "target_denied")


def _nfc(text: str) -> str:
    return unicodedata.normalize("NFC", text)


def _nfd(path: Path) -> Path:
    return Path(unicodedata.normalize("NFD", str(path)))


@pytest.fixture
def host(tmp_path):
    home = tmp_path / _nfc("jürgen") / "u"
    hermes = home / ".hermes"
    (hermes / "profiles" / "work").mkdir(parents=True)
    cfg = PolicyConfig.from_options(
        Options(allow_roots=("~",)),
        home=home,
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=True,
        target_os="macos",
    )
    assert _nfd(home) != home
    return home, hermes, cfg


@pytest.mark.parametrize(
    "rel", [".hermes/x", ".hermes/profiles/work/config.yaml", ".ssh/authorized_keys", "bin/x", ".bashrc"]
)
def test_nfd_spelling_of_a_denied_target_is_denied(rel, host):
    """★ Target side: the NFD spelling of ``<home>/<rel>`` meets the NFC home and Hermes root."""
    home, _, cfg = host
    assert policy.check_target_dest(_nfd(home / rel), cfg, recorded=False, local_choice=True) == DENIED


def test_nfd_spelling_of_the_hermes_home_is_protected_at_the_source(host):
    """★ Source side: ``is_hermes_protected`` on the NFD spelling of the Hermes home."""
    _, hermes, cfg = host
    assert policy.is_hermes_protected(_nfd(hermes / "state.db"), cfg)
    assert policy.is_hermes_protected(_nfd(hermes / "profiles" / "work" / ".env"), cfg)


def test_nfd_spelling_of_an_ordinary_path_stays_allowed(host):
    home, _, cfg = host
    assert policy.check_target_dest(_nfd(home / "proj"), cfg, recorded=False, local_choice=False) == Verdict(True, None)
