# Archived: the pre-merge fail-closed patch series

This directory is history, not a build path.

`ctx_checkpoint`'s fail-closed checkpoint contract used to exist only as a
patch series against pinned Hermes releases, because the host side was not
upstream yet. It is upstream now: issue
[NousResearch/hermes-agent#93986](https://github.com/NousResearch/hermes-agent/issues/93986)
→ PR
[#93996](https://github.com/NousResearch/hermes-agent/pull/93996)
→ merged 2026-08-25 as
[#94639](https://github.com/NousResearch/hermes-agent/pull/94639),
tip commit `1ee524f77d1c84a6ee1ae58341e633d8a9b6ef1d`.

Upstream renumbered the contract while merging: **API v1 is the implicit
historical best-effort behaviour, v2 is the opt-in fail-closed contract**
(`PRE_COMPRESS_CHECKPOINT_API_VERSION = 2` in `agent/memory_provider.py`).
Micro-compaction is bound to `checkpoint_required` as well
(`agent/turn_finalizer.py`). The archived patch below still speaks the
pre-merge numbering (API v1 == fail-closed).

**Do not apply this series to Hermes `main` at or after `1ee524f77d`.** The
host already carries the contract there; re-applying would conflict, and the
version numbers it writes are the ones upstream retired.

## What is kept here, and why

| Path | What it is |
|------|------------|
| `v2026.8.19/` | The last patch series: one reviewed patch plus SHA-256 manifests of the touched files before (`baseline.sha256`) and after (`patched.sha256`) patching. |
| `build-image.sh` | The tool that drove it — clone a pinned tag, verify the baseline manifest, patch, verify, build. Frozen; its `PATCH_ROOT` now points at this archive directory. |

`v2026.8.19` (`fcbd1076a93841fa88855acce810e342a5b78101`) is the last upstream
release cut **before** the contract landed. The series is preserved so that a
host pinned to that tag — or any earlier one — can still be reconstructed and
audited, and so the reviewed pre-merge form of the change stays readable next
to the merged one.

For current installation on a merged host, see `../../README.md`.
