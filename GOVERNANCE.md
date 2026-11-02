# Governance

How decisions are made in ctx, who may do what, and how code reaches `root`.
The short version: everything lands through a reviewed pull request, the
maintainer cuts releases, and write access is a tool for working in this
repository, not a way around review.

## Roles

| Role | Who | May |
|---|---|---|
| **Maintainer** | [@GottZ](https://github.com/GottZ) (owner) | everything below, plus: merge pull requests, cut release tags, change repository settings and rulesets, grant and revoke access, publish security advisories |
| **Collaborator** (GitHub role *write*) | invited by the maintainer; see [Becoming a collaborator](#becoming-a-collaborator) | push branches to this repository (no fork needed), review and approve pull requests, triage issues, apply labels, approve CI runs of first-time contributors, re-run workflows |
| **Contributor** | anyone | fork, open pull requests, report issues, join discussions |

Write access does **not** include pushing to `root`, merging without the
required review, creating or moving `v*` tags, or changing settings. The
repository rulesets enforce that for every account, the maintainer's included.

## How `root` changes

`root` is the main branch and the only release line. Two GitHub rulesets
guard it and apply to all accounts.

1. **Pull request required.** Nothing is pushed to `root` directly; the
   branch moves only by merging a pull request. Force pushes and deletion
   are blocked.
2. **One approving review** from a code owner
   ([`.github/CODEOWNERS`](.github/CODEOWNERS)), given on the current head —
   a new push dismisses earlier approvals. Every review conversation is
   resolved before the merge.
3. **Required status checks** (exact context names): `Lint`, `Unit Tests`,
   `Unit Tests (windows, tagged packages)`, `Build`, `Web`,
   `Cross-compile (darwin/arm64)`, `Cross-compile (windows/amd64)`,
   `Integration Tests (handler|store|guard|rest|heavy|distill)` and
   `PR title` — the `ci.yml` jobs that run on every pull request. Label-gated
   and nightly jobs are not required.
4. **Merge method**: *merge commit* (default) or *squash*. Rebase merging is
   off because it rewrites the contributor's commits and drops their
   signatures. The pull request title becomes the merge commit subject, so
   it follows `type(scope): summary` like a commit message (checked by the
   `PR title` workflow); the pull request body becomes the merge commit
   body. `git log --first-parent root` therefore reads as one entry per
   pull request. The source branch is deleted on merge.
5. **Who merges**: the maintainer. Collaborators approve; merging a pull
   request means taking responsibility for it on the release line.

**Maintainer escape hatch.** The maintainer's own pull requests need the
same approval, and an approval counts only from a code owner — with
`* @GottZ` as the sole owner, no one else can give it. Until a second code
owner is listed in [`.github/CODEOWNERS`](.github/CODEOWNERS), the maintainer
may merge an unapproved pull request through the ruleset bypass; GitHub
records the bypass. That is a stopgap, not a second path: the pull request,
the CI checks and the public review thread still happen. The bypass is
removed in the same change that adds the second code owner.

## Releases

A release is an annotated `v*` tag on a commit of `root`, pushed by the
maintainer; the `Release` workflow builds the binaries and publishes the
GitHub Release from the tag annotation. The maintainer signs tags with their
SSH key; the tag ruleset enforces who may create a tag, not its signature.

- Tags are cut on merge commits that CI has already judged, after the
  merge — never on a pull request branch.
- Versioning: a `feat` lands as a minor release, fixes as a patch, a
  breaking change as a major. Several merged pull requests may share one
  release.
- `v*` tags are created, moved or deleted only by the maintainer (tag
  ruleset); a published tag never changes.
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
