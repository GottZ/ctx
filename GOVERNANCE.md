# Governance

How decisions are made in ctx, who may do what, and how code reaches `root`.
The short version: everything lands through a reviewed pull request, the
maintainer cuts releases, and write access is a tool for working in this
repository, not a way around review.

## Roles

| Role | Who | May |
|---|---|---|
| **Maintainer** | [@GottZ](https://github.com/GottZ) (owner) | everything below, plus: merge contributors' pull requests, cut release tags, change repository settings and rulesets, grant and revoke access, publish security advisories |
| **Code owner** | [Damien Moon](https://github.com/DamieMoon), [derLonius](https://github.com/W4-NERF) and the maintainer, per [`.github/CODEOWNERS`](.github/CODEOWNERS) | a collaborator whose approval satisfies the review requirement on `root`; requested for review automatically; merges their own pull request once it is approved (see *Who merges*) |
| **Collaborator** (GitHub role *write*) | invited by the maintainer; see [Becoming a collaborator](#becoming-a-collaborator) | push branches to this repository (no fork needed), review pull requests, triage issues, apply labels, approve CI runs of first-time contributors, re-run workflows |
| **Contributor** | anyone | fork, open pull requests, report issues, join discussions |

Write access does **not** include pushing to `root`, merging without the
required review, creating or moving `v*` tags, or changing settings. The
repository rulesets enforce that for every account — with one exception,
the repository-admin bypass described under *Maintainer escape hatch* and
*Releases*: it lets the maintainer merge a pull request past the whole
`root` ruleset and create tags, never push to `root`.

## How `root` changes

`root` is the main branch and the only release line. One GitHub ruleset
guards it (id 25758355, target: the default branch) and applies to all
accounts, the admin bypass excepted (see below); the second ruleset
(id 26378757) covers `v*` tags and is described under *Releases*.

1. **Pull request required.** Nothing is pushed to `root` directly; the
   branch moves only by merging a pull request. Force pushes and deletion
   are blocked.
2. **One approving review** from a code owner
   ([`.github/CODEOWNERS`](.github/CODEOWNERS): GottZ, Damien Moon and
   derLonius), given on the current head: a new push dismisses earlier
   approvals, and the approval that counts must come from someone other
   than the person who pushed last. A pull request's author cannot approve
   it at all. Beyond code ownership, that is all GitHub checks about who
   approves — the last-push rule looks at the last pusher only, so an owner
   who pushed to a branch earlier can give the counting approval once
   someone else has pushed after them. The rest is convention: **an owner
   does not approve a pull request they pushed to**, so every owner's
   commits, including hardening pushed onto a contributor's branch, are
   approved by another owner — the maintainer's work like anyone else's.
   Every review conversation is resolved before the merge.
3. **Required status checks** (exact context names): `Lint`, `Unit Tests`,
   `Unit Tests (windows, tagged packages)`, `Build`, `Web`,
   `Cross-compile (darwin/arm64)`, `Cross-compile (windows/amd64)`,
   `Integration Tests (handler|store|guard|rest|heavy|distill)` and
   `PR title` — the `ci.yml` jobs that run on every pull request. Label-gated
   and nightly jobs are not required.
4. **Merge method**: *merge commit* (default) or *squash*. A merge commit
   keeps the contributor's commits and their signatures; a squash creates one
   new commit (signed by GitHub) and is for branches whose history is not
   worth keeping. Rebase merging is off because it rewrites every commit and
   drops the signatures without saying so. The pull request title becomes
   the merge commit subject, so
   it follows `type(scope): summary` like a commit message (checked by the
   `PR title` workflow); the pull request body becomes the merge commit
   body. `git log --first-parent root` therefore reads as one entry per
   pull request. The source branch is deleted on merge.
5. **Who merges** — a convention, not a ruleset rule: the rulesets say what
   must be true before a merge, not who clicks. Contributors cannot merge
   (no write); the maintainer merges their pull requests. An owner's own
   pull request is merged by its author once the approval is in. Merging
   means taking responsibility for the change on the release line.

**Maintainer escape hatch.** The ruleset lets repository admins merge a
pull request through a bypass (bypass mode *pull requests only* — a direct
push stays blocked). A bypass merge skips the whole root ruleset for that
one merge: the approval, the required checks (`PR title` included), thread
resolution and the last-push rule — not only the missing approval. GitHub
records every such merge as a bypass. It exists for
the case where no other owner is available in time, and it is used that
way: as the exception the merge commit's audit trail shows, not as the
maintainer's regular path. It is removed by a change to the ruleset once the
other owners review regularly.

## Releases

A release is an annotated `v*` tag on a commit of `root`, pushed by the
maintainer; the `Release` workflow builds the binaries and publishes the
GitHub Release from the tag annotation. The maintainer signs tags with their
SSH key; the tag ruleset enforces who may create a tag, not its signature.

- Tags are cut on merge commits that CI has already judged, after the
  merge — never on a pull request branch. The authoritative gate is the
  `Release` workflow's `verify` job: it fails before anything is built
  unless the tag is a version tag (`vMAJOR.MINOR.PATCH`), annotated with
  release notes, the checkout is the tag's commit, and that commit is on
  `root`; a manual dispatch resolves `refs/tags/<input>` only. The pre-push
  hook applies the same rules first, reading the remote `root` tip live, so
  the maintainer sees the error before the push — but a hook can be skipped,
  the workflow cannot.
- The live tier is green on the tagged commit: `ci.yml` runs the
  `Web Live Tier` job (a throwaway ctxd + Postgres, production write paths)
  on every push to `root`, and `verify` requires a successful run on the
  exact commit — the repository's "CI is truth" rule extended to the live
  tier. The nightly run of the same job is the daily drift signal, not the
  gate. Tags are cut on the tip of `root`: a second merge while the first
  one's run is still going cancels that run, so an earlier commit has no
  verdict unless its run is re-run by hand. A re-run keeps the original
  event and ref, so it lands in the concurrency group of `root`'s push
  runs and cancels the one in progress there: re-run only once no newer
  `ci.yml` run on `root` is queued or in progress. If a re-run cancels a
  newer run anyway, no image tag moves to the older commit — `ci.yml`
  publishes images one job at a time across all refs, pushes only the
  per-commit `sha-` tag with the build, and promotes `latest` and the date
  tag only after re-reading the tip of `root` — but the newer commit then
  needs a re-run of its own for its verdict and its image. For a run from
  before that guard, re-run only its `Web Live Tier` job, never the whole
  run.
- Versioning: a `feat` lands as a minor release, fixes as a patch, a
  breaking change as a major. Several merged pull requests may share one
  release.
- `v*` tags are created, moved or deleted only by the maintainer: the tag
  ruleset blocks creation, update and deletion for everyone, and the
  maintainer acts through its admin bypass. A published tag never changes.
- A published release never changes either: both `verify` and `publish`
  refuse a tag that already has a release (checked right before the upload,
  so a re-run of single jobs cannot slip past a reused `verify` result),
  the upload never overwrites an existing asset, and manual dispatches are
  limited to the repository owner. The repository-level *immutable
  releases* setting is not enabled yet.
- Only the newest minor receives fixes ([SECURITY.md](SECURITY.md)).

## Decisions

- Direction and design are discussed in
  [GitHub Discussions](https://github.com/GottZ/ctx/discussions); concrete
  work is an issue; the change itself is argued in its pull request.
- The maintainer decides where there is no consensus. Decisions with lasting
  effect are written down where they apply — `docs/`, this file,
  [CONTRIBUTING.md](CONTRIBUTING.md) — not only in a thread.
- Review findings name the evidence they rest on (a run, a read code path,
  or a hypothesis), and a finding without a reachable path is a note, not a
  blocker.

## Becoming a collaborator

Write access is offered by the maintainer to contributors whose pull
requests have landed and who are around to review others'. It is a practical
grant — branches in this repository, reviewing, triage — not a title, and it
is removed when unused. Collaborators keep the same rules as everyone else
for `root` and for tags.

## AI agents

Much of the implementation and the first review pass are done by AI agents
working under the methodology in
[docs/using-ctx-effectively.md](docs/using-ctx-effectively.md). Agent work is
attributed in commit trailers; the human who merges is accountable for it. A
review by an agent is a review source, not an approval — approvals come from
accounts in the roles above.

## Security

Vulnerabilities are reported privately ([SECURITY.md](SECURITY.md)) and fixed
on `root` before disclosure. The fix still goes through a pull request; where
the diff would reveal the issue early, it is prepared in the advisory's
temporary private fork and merged when the advisory is published.

## Changing this document

Through a pull request, like everything else.
