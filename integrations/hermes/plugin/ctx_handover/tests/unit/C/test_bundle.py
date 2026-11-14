"""Offline bundle: writer (``BundleTarget``) and reader (``BundleSource``) (DESIGN §5.4; SEC 1.6, 2.6; T4)."""

from __future__ import annotations

import hashlib
import inspect
import io
import json
import os
import stat
import tarfile
import time
from dataclasses import replace

import pytest

from ctx_handover.engine import bundle, contracts, plan
from ctx_handover.engine.blocks import BLOCK_SIZE
from ctx_handover.engine.bundle import BundleSource, BundleTarget, read_manifest
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.errors import BadRequest, ConflictError, Unauthorized
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import canonical_bytes, from_wire
from ctx_handover.engine.model import (
    CapsuleInfo,
    CommitAck,
    EnvFacts,
    FinalPlan,
    Include,
    ItemKind,
    ItemRole,
    Mode,
    OpState,
    Options,
    Origin,
    PackInfo,
    Peer,
    PlanItem,
    PrepareRequest,
    RefineReport,
    ResourcePlan,
    Right,
    SessionRef,
)
from ctx_handover.tests.fakes.blocks_fake import MemoryBlockStore
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.C.helpers import _NullReporter, sample_facts
from ctx_handover.tests.unit.C.ledger_stub import make_ledger

M = __import__("ctx_handover.engine.model", fromlist=["m"])


def _noise(n: int, seed: int) -> bytes:
    out = bytearray()
    i = 0
    while len(out) < n:
        out += hashlib.sha256(f"{seed}:{i}".encode()).digest()
        i += 1
    return bytes(out[:n])


def job_ctx(job_id: str | None = None) -> JobContext:
    return JobContext(
        job_id=job_id or ulid(),
        mode=Mode.EXPORT,
        peer=None,
        options=Options(),
        epoch=1,
        reporter=_NullReporter(),
        cancelled=lambda: False,
        created_at=time.time(),
    )


class Exporter:
    def __init__(self, tmp_path, name="exporter"):
        self.ledger = make_ledger(tmp_path / name / "ledger.sqlite3")
        self.identity = new_identity(self.ledger)
        self.source = MemoryBlockStore()


def _move(src: MemoryBlockStore, sink, object_ids):
    ms = src.manifests(object_ids)
    sink.put_manifests(ms)
    ids = [b for m in ms for b in m.blocks]
    missing = sink.missing(ids)
    _accepted, rejected = sink.write_blocks((b, src.get(b)) for b in missing)
    assert rejected == []
    assert sink.missing(ids) == []


def make_bundle(tmp_path, exp: Exporter, *, as_dir=False, retire=True, name="out"):
    out = tmp_path / (f"{name}.d" if as_dir else f"{name}.ctxh.tar")
    target = BundleTarget(
        out, exp.identity, as_dir=as_dir, declared_facts=None, ledger=exp.ledger, retire=retire, exporter_name="server"
    )
    assert isinstance(target, contracts.TargetAPI)
    job = job_ctx()
    target.open_job(job)
    facts = sample_facts(M, "server")
    tfacts = target.facts(job, facts)
    assert isinstance(tfacts, EnvFacts)
    item = PlanItem(
        item_id="i1",
        kind=ItemKind.FILE_TREE,
        source_path="/w/proj",
        role=ItemRole.PRIMARY,
        origin=Origin.CWD,
        include=Include.YES,
        reasons=("cwd",),
        size_estimate=10,
    )
    plan = ResourcePlan(
        job_id=job.job_id,
        revision=(1, 2),
        items=(item,),
        not_transferred=(),
        processes=(),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=(),
        env_vars_needed=(),
        notes_for_target="",
        approx_tokens=5,
    )
    final = target.negotiate(job, plan)
    assert [d.item_id for d in final.decisions] == ["i1"]
    capsule_obj = exp.source.put_bytes(_noise(BLOCK_SIZE + 77, 1), "application/json")
    pack_obj = exp.source.put_bytes(_noise(3000, 2), "application/json")
    _move(exp.source, target.blocks(), [capsule_obj.object_id, pack_obj.object_id])
    capsule = CapsuleInfo(
        capsule_id=capsule_obj.object_id,
        size=capsule_obj.size,
        segments=1,
        rows=3,
        max_segment_bytes=capsule_obj.size,
        approx_tokens=5,
        h_all="ha",
        h_rows="hr",
        h_model="hm",
        revision=(1, 2),
    )
    op = target.start_stage_context(job, capsule)
    assert op.state == OpState.DONE, op
    req = PrepareRequest(
        capsule=capsule,
        pack=PackInfo(pack_id=pack_obj.object_id, size=pack_obj.size),
        final_plan_id=final.plan_id,
        source_facts=facts,
        note_parts={"title": "My session"},
    )
    op = target.start_prepare(job, req)
    assert op.state == OpState.DONE, op
    op = target.start_commit(job)
    assert op.state == OpState.DONE, op
    ack = from_wire(CommitAck, op.result)
    assert str(out) in " ".join(ack.warnings)
    return out, job, capsule_obj, pack_obj, final


@pytest.mark.parametrize("as_dir", [False, True])
def test_bundle_write_read_roundtrip(tmp_path, as_dir):
    exp = Exporter(tmp_path)
    out, job, capsule_obj, pack_obj, final = make_bundle(tmp_path, exp, as_dir=as_dir)
    assert out.exists()
    assert not (tmp_path / f"{out.name}.partial").exists()
    if as_dir:
        assert stat.S_IMODE(out.stat().st_mode) == 0o700
        assert (out / "CTXH-BUNDLE").read_bytes() == b"ctxh-bundle 1\n"
    else:
        assert stat.S_IMODE(out.stat().st_mode) == 0o600
        with tarfile.open(out, "r:") as tf:
            names = tf.getnames()
        assert names[:3] == ["CTXH-BUNDLE", "manifest.json", "manifest.sig"]
        assert all(n.startswith(("manifests/", "blocks/")) for n in names[3:])
    manifest, verified = read_manifest(out, trusted_pubkeys={exp.identity.public_b64()})
    assert verified
    assert manifest["kind"] == "ctxh.bundle" and manifest["retire"] is True
    assert manifest["job_id"] == job.job_id
    assert manifest["exporter"]["pubkey"] == exp.identity.public_b64()
    # capsule + pack + the final-plan and facts documents the writer adds
    assert manifest["totals"]["blocks"] == len(capsule_obj.blocks) + len(pack_obj.blocks) + 2
    assert set(manifest["objects"]) >= {capsule_obj.object_id, pack_obj.object_id}

    importer_ledger = make_ledger(tmp_path / "importer" / "ledger.sqlite3")
    src = BundleSource(out, trusted_pubkeys={exp.identity.public_b64()}, trust_unsigned=False, ledger=importer_ledger)
    assert isinstance(src, contracts.SourceAPI)
    assert src.verified and src.signer == "trusted key"
    ijob = job_ctx()
    assert src.facts(ijob).instance_name == "server"
    plan_op = src.start_plan(ijob, sample_facts(M, "laptop"), SessionRef())
    assert from_wire(ResourcePlan, plan_op.result) == final.plan
    info = src.lock(ijob, SessionRef(), (1, 2))
    assert info.title == "My session"
    assert from_wire(CapsuleInfo, src.start_export(ijob).result).capsule_id == capsule_obj.object_id
    assert from_wire(PackInfo, src.start_pack(ijob, final).result).pack_id == pack_obj.object_id
    blocks = src.blocks()
    assert blocks.manifests([capsule_obj.object_id]) == [capsule_obj]
    data = b"".join(raw for _i, raw in blocks.read_blocks(list(capsule_obj.blocks)))
    assert data == exp.source.open_object(capsule_obj.object_id).read()
    ack = CommitAck(
        session_id="20261109_120000_abcdef",
        root_id="r",
        title=None,
        dests=(),
        path_map=(),
        model=None,
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )
    assert src.start_retire(ijob, ack).state == OpState.DONE
    assert importer_ledger.bundle_import_seen(job.job_id) == "20261109_120000_abcdef"


def test_bundle_signature_trust_only_stored_keys(tmp_path):
    """★ ``exporter.pubkey`` alone never verifies; a stored, non-revoked peer key does; revoked ⇒ unverified
    (SEC 1.6, T4, hardening #4).

    Red proof: mutant = ``read_manifest`` adds ``manifest["exporter"]["pubkey"]`` to the trusted keys → the bundle
    of an unknown exporter is reported verified.
    """
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp)
    importer = make_ledger(tmp_path / "importer" / "ledger.sqlite3")
    _manifest, verified = read_manifest(out)
    assert verified is False
    with pytest.raises(Unauthorized):
        BundleSource(out, trusted_pubkeys=set(), trust_unsigned=False, ledger=importer)
    unverified = BundleSource(out, trusted_pubkeys=set(), trust_unsigned=True, ledger=importer)
    assert unverified.verified is False and unverified.signer is None
    peer = Peer(
        name="server",
        peer_id=ulid(),
        direction="dial",
        url="https://s.example",
        pubkey=exp.identity.public_b64(),
        rights=frozenset({Right.PULL}),
        created_at=time.time(),
    )
    importer.upsert_peer(peer)
    trusted = BundleSource(out, trusted_pubkeys=set(), trust_unsigned=False, ledger=importer)
    assert trusted.verified and trusted.signer == "server"
    importer.revoke_peer("server")
    assert BundleSource(out, trusted_pubkeys=set(), trust_unsigned=True, ledger=importer).verified is False


def _rewrite_tar(src, dst, *, edit=None, extra=()):
    """Copy a bundle tar, optionally replacing member bytes (``edit(name, data) -> data``) and appending
    ``extra`` TarInfo/bytes pairs."""
    with tarfile.open(src, "r:") as tin, tarfile.open(dst, "w", format=tarfile.PAX_FORMAT) as tout:
        for member in tin.getmembers():
            data = tin.extractfile(member).read()
            if edit is not None:
                data = edit(member.name, data)
            info = tarfile.TarInfo(member.name)
            info.size = len(data)
            info.mode = 0o600
            tout.addfile(info, io.BytesIO(data))
        for info, data in extra:
            tout.addfile(info, io.BytesIO(data) if data is not None else None)


def _info(name, *, kind=tarfile.REGTYPE, size=0, link=""):
    info = tarfile.TarInfo(name)
    info.type = kind
    info.size = size
    info.linkname = link
    info.mode = 0o600
    return info


def test_bundle_rejects_unexpected_members(tmp_path):
    """★ Only the exact member names of DESIGN §5.4 as regular files; links, traversal, extra or duplicated
    members and deeper paths are refused before anything is used (SEC 2.6, T4, T6).

    Red proof: mutant = member-name allowlist check in ``_check_member`` skipped → the tar with ``evil.txt``
    opens without ``BadRequest``.
    """
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp)
    with tarfile.open(out) as tf:
        block = next(n for n in tf.getnames() if n.startswith("blocks/"))
    cases = {
        "extra": [(_info("evil.txt", size=1), b"x")],
        "traversal": [(_info("../escape", size=1), b"x")],
        "absolute": [(_info("/etc/x", size=1), b"x")],
        "deep": [(_info("blocks/ab/cd/" + "0" * 60, size=1), b"x")],
        "symlink": [(_info("blocks/zz/" + "0" * 62, kind=tarfile.SYMTYPE, link="/etc/passwd"), None)],
        "hardlink": [(_info("blocks/ff/" + "0" * 62, kind=tarfile.LNKTYPE, link="manifest.json"), None)],
        "directory": [(_info("blocks", kind=tarfile.DIRTYPE), None)],
        "duplicate": [(_info(block, size=1), b"x")],
        "fifo": [(_info("blocks/ee/" + "0" * 62, kind=tarfile.FIFOTYPE), None)],
    }
    trusted = {exp.identity.public_b64()}
    for name, extra in cases.items():
        bad = tmp_path / f"bad-{name}.ctxh.tar"
        _rewrite_tar(out, bad, extra=extra)
        with pytest.raises(BadRequest):
            read_manifest(bad, trusted_pubkeys=trusted)
        with pytest.raises(BadRequest):
            BundleSource(bad, trusted_pubkeys=trusted, trust_unsigned=True, ledger=exp.ledger)
    # a directory bundle with a symlink is refused the same way
    out_dir, *_ = make_bundle(tmp_path, exp, as_dir=True, name="dir")
    os.symlink("/etc/hostname", out_dir / "blocks" / "link")
    with pytest.raises(BadRequest):
        read_manifest(out_dir, trusted_pubkeys=trusted)


def test_bundle_totals_mismatch(tmp_path):
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp)
    edited = tmp_path / "edited.ctxh.tar"
    holder = {}

    def edit(name, data):
        if name == "manifest.json":
            doc = json.loads(data)
            doc["totals"]["bytes"] += 1
            holder["bytes"] = canonical_bytes(doc)
            return holder["bytes"]
        if name == "manifest.sig":
            return exp.identity.sign(holder["bytes"]).encode()
        return data

    _rewrite_tar(out, edited, edit=edit)
    with pytest.raises(BadRequest) as info:
        read_manifest(edited, trusted_pubkeys={exp.identity.public_b64()})
    assert "totals" in info.value.message


def test_bundle_tampered_object_manifest_rejected(tmp_path):
    exp = Exporter(tmp_path)
    out, _job, capsule_obj, *_ = make_bundle(tmp_path, exp)
    edited = tmp_path / "edited.ctxh.tar"
    target_name = f"manifests/{capsule_obj.object_id[:2]}/{capsule_obj.object_id[2:]}.json"

    def edit(name, data):
        if name == target_name:
            doc = json.loads(data)
            doc["blocks"] = list(reversed(doc["blocks"]))
            return canonical_bytes(doc)
        return data

    _rewrite_tar(out, edited, edit=edit)
    with pytest.raises(BadRequest):
        read_manifest(edited, trusted_pubkeys={exp.identity.public_b64()})


def test_bundle_tampered_block_rejected_on_read(tmp_path):
    exp = Exporter(tmp_path)
    out, _job, capsule_obj, *_ = make_bundle(tmp_path, exp)
    edited = tmp_path / "edited.ctxh.tar"
    victim = capsule_obj.blocks[0]

    def edit(name, data):
        return b"X" + data[1:] if name == f"blocks/{victim[:2]}/{victim[2:]}" else data

    _rewrite_tar(out, edited, edit=edit)
    src = BundleSource(edited, trusted_pubkeys={exp.identity.public_b64()}, trust_unsigned=False, ledger=exp.ledger)
    with pytest.raises(bundle.BlockCorrupt):
        list(src.blocks().read_blocks([victim]))


def test_bundle_reimport_needs_again(tmp_path):
    exp = Exporter(tmp_path)
    out, _job, *_ = make_bundle(tmp_path, exp)
    importer = make_ledger(tmp_path / "importer" / "ledger.sqlite3")
    trusted = {exp.identity.public_b64()}
    first = BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer)
    ack = CommitAck(
        session_id="s1",
        root_id="r",
        title=None,
        dests=(),
        path_map=(),
        model=None,
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )
    first.start_retire(job_ctx(), ack)
    with pytest.raises(ConflictError) as info:
        BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer)
    assert "--again" in info.value.message and "s1" in info.value.message
    again = BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer, again=True)
    assert again.reimport_of == "s1"


def test_bundle_partial_dir_private_and_abort_cleans(tmp_path):
    """Hardening #20 (SEC 6.1): the staging directory is 0700 until commit; abort removes it."""
    exp = Exporter(tmp_path)
    out = tmp_path / "x.ctxh.tar"
    target = BundleTarget(out, exp.identity, as_dir=False, declared_facts=None, ledger=exp.ledger)
    job = job_ctx()
    target.open_job(job)
    obj = exp.source.put_bytes(b"abc", "application/json")
    _move(exp.source, target.blocks(), [obj.object_id])
    partial = tmp_path / "x.ctxh.tar.partial"
    assert stat.S_IMODE(partial.stat().st_mode) == 0o700
    for path in partial.rglob("*"):
        assert stat.S_IMODE(path.stat().st_mode) == (0o700 if path.is_dir() else 0o600), path
    target.abort(job, "user")
    assert not partial.exists()
    assert not out.exists()


def test_bundle_copy_flag_and_refused_continue(tmp_path):
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp, retire=False, name="copy")
    manifest, _ = read_manifest(out, trusted_pubkeys={exp.identity.public_b64()})
    assert manifest["retire"] is False
    src = BundleSource(out, trusted_pubkeys={exp.identity.public_b64()}, trust_unsigned=False, ledger=exp.ledger)
    assert src.is_copy is True


def _ack(session_id: str = "s1") -> CommitAck:
    return CommitAck(
        session_id=session_id,
        root_id="r",
        title=None,
        dests=(),
        path_map=(),
        model=None,
        offline_capable=True,
        warnings=(),
        committed_at=1.0,
    )


def test_bundle_plan_id_follows_the_planning_rule(tmp_path):
    """★ X02: ``BundleTarget.negotiate`` sets ``plan_id = plan.plan_id(plan.final_doc(final))`` (DESIGN §8.6) — for
    its own fresh-directory decision and for an injected negotiation that returns a stale id; the bundle manifest
    and the stored FinalPlan document carry the same id.

    Red proof: mutant = the layer-1 rule ``doc_id(to_wire(final))`` over the document with ``plan_id=""`` → the id
    differs from ``plan.plan_id(plan.final_doc(final))``.
    """
    exp = Exporter(tmp_path)
    out, _job, _capsule, _pack, final = make_bundle(tmp_path, exp)
    rule = plan.plan_id(plan.final_doc(final))
    assert final.plan_id == rule
    trusted = {exp.identity.public_b64()}
    manifest, _verified = read_manifest(out, trusted_pubkeys=trusted)
    assert manifest["plan_id"] == rule
    src = BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=exp.ledger)
    stored = from_wire(FinalPlan, json.loads(src.blocks().object_bytes(manifest["final_plan_id"])))
    src.close()
    assert stored.plan_id == rule == plan.plan_id(plan.final_doc(stored))
    injected = BundleTarget(
        tmp_path / "injected.ctxh.tar",
        exp.identity,
        as_dir=False,
        declared_facts=None,
        ledger=exp.ledger,
        negotiate=lambda p, f: replace(final, plan_id="stale"),
    )
    job = job_ctx()
    injected.open_job(job)
    injected.facts(job, sample_facts(M, "server"))
    assert injected.negotiate(job, final.plan).plan_id == rule
    injected.abort(job, "test")


def test_bundle_retire_and_again_parameters(tmp_path):
    """X23 / W058 / W059: the composition root passes ``BundleTarget(…, retire=not options.keep_source)`` and
    ``BundleSource(…, trusted_pubkeys=…, trust_unsigned=…, again=options.again)``. The keyword parameters exist with
    the defaults of O3 and SK L5 and act: ``retire`` decides ``manifest.retire`` and ``is_copy``; ``again`` admits a
    second import of a bundle this profile imported before."""
    target_params = inspect.signature(BundleTarget).parameters
    source_params = inspect.signature(BundleSource).parameters
    assert target_params["retire"].kind is inspect.Parameter.KEYWORD_ONLY and target_params["retire"].default is True
    assert source_params["again"].kind is inspect.Parameter.KEYWORD_ONLY and source_params["again"].default is False
    for name in ("trusted_pubkeys", "trust_unsigned", "ledger"):
        assert source_params[name].kind is inspect.Parameter.KEYWORD_ONLY
    exp = Exporter(tmp_path)
    trusted = {exp.identity.public_b64()}
    for retire in (True, False):
        out, *_ = make_bundle(tmp_path, exp, retire=retire, name=f"retire-{retire}")
        manifest, verified = read_manifest(out, trusted_pubkeys=trusted)
        assert manifest["retire"] is retire and verified
        importer = make_ledger(tmp_path / f"importer-{retire}" / "ledger.sqlite3")
        first = BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer)
        assert first.is_copy is (not retire)
        first.start_retire(job_ctx(), _ack("s1"))
        first.close()
        with pytest.raises(ConflictError):
            BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer, again=False)
        again = BundleSource(out, trusted_pubkeys=trusted, trust_unsigned=False, ledger=importer, again=True)
        assert again.reimport_of == "s1" and again.is_copy is (not retire)
        again.close()


def test_sec_manifest_size_cap(tmp_path, monkeypatch):
    """T7: a bundle manifest above the cap is refused from the tar header, before its bytes are read."""
    exp = Exporter(tmp_path)
    out, *_ = make_bundle(tmp_path, exp)
    monkeypatch.setattr(bundle, "MAX_MANIFEST_BYTES", 64)
    with pytest.raises(BadRequest) as info:
        read_manifest(out, trusted_pubkeys={exp.identity.public_b64()})
    assert info.value.details.get("reason") == "too_large"
