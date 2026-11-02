<!--
Title: `type(scope): summary` — it becomes the merge commit subject.
Types: feat fix docs test ci chore refactor perf build revert.
-->

## What this changes

<!-- Intent in one or two sentences, then what the diff does. Name the place
where behaviour changes on purpose. -->

## Why

<!-- The problem, the issue it closes (`Closes #N`), the alternative you
rejected and why. -->

## Verification

<!-- What you ran and on which commit: `go test ./... -short`, `bun run check`,
manual steps, live checks. Name what you did NOT run. -->

## Checklist

- [ ] One concern per pull request
- [ ] Commits follow `type(scope): summary`; the repository hooks were enabled (`git config core.hooksPath .hooks`)
- [ ] Tests cover the change; a bug fix carries a test that fails without it
- [ ] User-visible behaviour is documented (`README.md` or `docs/*.md`)
- [ ] Migrations are append-only; a new one carries the regenerated schema manifest
- [ ] **Allow edits by maintainers** is on
