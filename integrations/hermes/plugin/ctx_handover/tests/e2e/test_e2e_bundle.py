"""Layer 3 — offline bundle (TESTPLAN §4 E11; DESIGN §5.4, O3).

Known red before the run: ``reports/L2-red.md`` V24 (``BundleTarget`` keeps its state only in memory, so a CLI
export ends ``in_doubt`` and waits up to ``stall_timeout_min``). The test therefore sets A's
``stall_timeout_min`` to 1 and bounds the export call to 300 s; the setting is restored afterwards.

Runtime estimate: 1–2 min when the export works; up to ≈ 6 min while V24 is open (bounded).
"""

from __future__ import annotations

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


@pytest.mark.e2e_timeout(900)
def test_e11_bundle_export_import(lab, ev):
    """E11: ``/handover export`` on A (CLI, ``--for laptop``: signed for the paired device) writes a single
    ``.ctxh.tar``; ``import`` on B verifies it (origin "verified", paired exporter) and lands the session; a tampered
    copy fails; a second import of the same bundle needs ``--again``."""
    src = lab.make_source("a", "e11proj", turns=("bundle me",))
    hashes_a = lab.hashes("a", src.sid)
    folder = lab.handover_root("a") / f"e11-{lab.run_id}"
    folder.mkdir(parents=True, exist_ok=True)
    bundle = folder / "e11.ctxh.tar"
    with lab.preserved("a"):
        lab.setting("a", "stall_timeout_min", "1")
        exported = lab.handover(
            "a", "export", str(bundle), "--session", src.sid, "--for", _lab.LAPTOP, "--yes", timeout=300
        )
    ev.cli("export", exported)
    if exported.job_id:
        ev.write("job-a.json", lab.job_status("a", exported.job_id))
    ev.write("folder.json", sorted(p.name for p in folder.iterdir()))
    if exported.rc != 0:  # clean-up, not under test: the stuck export keeps the source locked (V24/L3-F06)
        ev.cli("cleanup-reclaim", lab.handover("a", "reclaim", src.sid, "--force", timeout=180))
    assert exported.rc == 0, exported.brief()
    assert bundle.is_file(), sorted(p.name for p in folder.iterdir())
    assert sorted(p.name for p in folder.iterdir()) == [bundle.name], "more than the single bundle file"

    imported = lab.handover("b", "import", str(bundle), "--yes", timeout=600)
    ev.cli("import", imported)
    assert imported.rc == 0, imported.brief()
    status = lab.job_status("b", imported.job_id)
    ev.write("job-b.json", status)
    job = status["job"]
    assert job.get("state") == "done", job
    origin = str(job.get("origin") or "")
    assert "verified" in origin.lower() and "unverified" not in origin.lower(), origin
    sid_b = str((job.get("ack") or {}).get("session_id") or "")
    hashes_b = lab.hashes("b", sid_b)
    for key in ("h_all", "h_rows", "h_model", "display_norm"):
        assert hashes_b[key] == hashes_a[key], key

    tampered = folder.parent / f"e11-{lab.run_id}-tampered.ctxh.tar"
    data = bytearray(bundle.read_bytes())
    data[len(data) // 2] ^= 0xFF
    tampered.write_bytes(bytes(data))
    bad = lab.handover("b", "import", str(tampered), "--yes", "--again", timeout=600)
    ev.cli("import-tampered", bad)
    assert bad.rc != 0, bad.brief()

    again = lab.handover("b", "import", str(bundle), "--yes", timeout=600)
    ev.cli("import-again-without-flag", again)
    assert again.rc != 0 and "--again" in again.text, again.brief()
    forced = lab.handover("b", "import", str(bundle), "--yes", "--again", timeout=600)
    ev.cli("import-again", forced)
    assert forced.rc == 0, forced.brief()
