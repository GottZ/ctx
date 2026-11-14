"""Layer R (red first) — README statements required by the layer-2 contract §2 F and §3 (named limits):
the MCP allowlist write of ``/handover setup``, the block-cache limit while a handover runs, the session limits
(older Hermes builds, typed messages, display after an undone message, a quiet pull
source) and the orphaned ``index.lock`` that ends in ``needs_attention``. Text present = test green."""

from __future__ import annotations

import re

import pytest

from ctx_handover.tests.unit.F._l2r_helpers import FLAT, near


def test_l2r_readme_names_the_mcp_allowlist_write():
    """§2 F (K-5): the disclosure says that ``/handover setup`` writes
    ``plugins.entries.ctx_handover.mcp_allowlist`` in ``config.yaml`` (``hostcompat.grant_mcp_server``)."""
    assert "plugins.entries.ctx_handover.mcp_allowlist" in FLAT
    assert near(FLAT, "plugins.entries.ctx_handover.mcp_allowlist", "/handover setup")


def test_l2r_readme_names_the_cache_limit_during_a_handover():
    """§2 F (W-3): while a handover runs, the block cache may grow beyond ``block_cache_gb`` — by at most the
    blocks of the last 24 h — and the next GC after the job ends clears it."""
    assert "block_cache_gb" in FLAT
    sentences = re.split(r"(?<=[.;])\s+", FLAT)
    hits = [
        s
        for s in sentences
        if "block_cache_gb" in s
        and re.search(r"\b(exceed|exceeds|above|beyond|over)\b", s)
        and re.search(r"24 ?h|24 hours|a day", s)
    ]
    assert hits, "no sentence names block_cache_gb, the overrun while a handover runs and the 24 h bound"
    assert near(FLAT, "block_cache_gb", "GC")


SESSION_LIMITS = {
    "older-builds": (
        "**Sessions from older Hermes builds.**",
        "the target refuses the late changes (`verify_failed`) and the handover starts over from a fresh export",
    ),
    "typed-messages": (
        "**Messages typed into a handed-over session.**",
        "Attempts that a context compaction moved into an earlier part of the session stay in its history",
    ),
    "undone-display": (
        "**Display after an undone message.**",
        "What the model sees is identical on both sides; undone messages themselves are not transferred.",
    ),
    "quiet-pull-source": (
        "**Pulling from a machine that goes quiet.**",
        # Spec correction by the integrator (layer I, decision 2): the setting is
        # `participant_idle_timeout_min` (settings.SCHEMA, plugin.yaml config_schema); `participant_idle_timeout`
        # names no key.
        "`participant_idle_timeout_min` (15 minutes by default), the source releases its locks",
    ),
}


@pytest.mark.parametrize("key", sorted(SESSION_LIMITS))
def test_l2r_readme_carries_the_session_limits_of_wp_b(key):
    """§2 F / §3: the README limits carry the agreed text (each paragraph with its lead-in
    and its decisive clause)."""
    lead, clause = SESSION_LIMITS[key]
    assert lead in FLAT, lead
    assert clause in FLAT, clause
    assert near(FLAT, lead, clause, window=900)


def test_l2r_readme_names_the_orphaned_index_lock():
    """§2 F / §3: a git process that died mid-operation leaves ``index.lock``; the plugin does not repair it, the
    continuation ends ``needs_attention`` — named as a limit."""
    assert "index.lock" in FLAT
    assert "needs_attention" in FLAT
    assert near(FLAT, "index.lock", "needs_attention")
