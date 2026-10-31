# Contributing to ctx

Thanks for taking the time. This page says where each kind of message goes and
what a pull request needs to get merged.

## Questions, ideas and feedback

Use [GitHub Discussions](https://github.com/GottZ/ctx/discussions):

- [Q&A](https://github.com/GottZ/ctx/discussions/categories/q-a) — how do I…, is this expected…
- [Ideas](https://github.com/GottZ/ctx/discussions/categories/ideas) — features, changes in direction, design feedback
- [General](https://github.com/GottZ/ctx/discussions/categories/general) — everything else, including how you use ctx

An idea that turns into concrete work becomes an issue.

## Bugs

Open an [issue](https://github.com/GottZ/ctx/issues/new/choose). A report that can be
acted on names:

- the version (`ctx version`, or the release tag / commit) and how ctx runs: compose
  stack, container image or bare binary
- what you did, what you expected, and what happened instead — exact commands and
  output, log lines from `docker compose logs ctx` where relevant
- whether it reproduces, and how often

Several unrelated findings are welcome in one issue (see #40); they are tracked as a
checklist there.

**Security vulnerabilities do not go into public issues.** Report them privately as
described in [SECURITY.md](SECURITY.md).

## Pull requests

1. Fork the repository and branch from `root` (the main branch).
2. Enable the git hooks once: `git config core.hooksPath .hooks`. They run
   golangci-lint on staged Go files and check the commit message rules below.
3. Make the change. Build and test as described in
   [docs/development.md](docs/development.md); at least:

   ```bash
   cd go && go build ./... && go test ./... -short
   ```

   Integration tests (`-tags=integration`) need Docker for testcontainers; CI runs them
   on every pull request.
4. Open the pull request against `root` and leave **Allow edits by maintainers** on.

### What gets a pull request merged

- **One concern per pull request.** A fix and an unrelated refactor are two PRs.
- **Commit messages** follow `type(scope): summary` — `feat`, `fix`, `docs`, `test`,
  `ci`, `chore`, `refactor`; the scope is the package or area (`fix(cli): …`,
  `feat(llm): …`). The body says why.
- **Features and schema changes update the documentation.** The commit-msg hook
  rejects a `feat:` commit or a change under `go/migrations/` that does not also stage
  `README.md` or a `docs/*.md` file.
- **Migrations are append-only.** Never edit a migration that has landed on `root`;
  add a new one. A new migration needs the regenerated schema manifest in the same
  commit — the pre-commit hook prints the command. The one exception is a shipped
  migration that cannot complete on some databases (no later migration would ever
  run there): amend it, and in the same commit
  - add the shipped file's sha256 to `go/migrations/amended.go`;
  - keep the shipped file byte for byte as
    `go/migrations/testdata/<name>.<first 8 hex>.sql.txt` (not `.sql`: the hook's
    `go/migrations/*.sql` pathspec reaches into subdirectories) and let a test show
    it failing where the amended file passes;
  - regenerate the manifest and compare. If only an image-driven
    `extensions.*.min_version` moves, do not commit that bump; the hook cannot tell
    a catalog-neutral (DML-only) amendment yet, so the commit takes the
    `--no-verify` path the hook names, after running its gates by hand, and the
    message states the regeneration result;
  - document the change for operators, as for any migration.
- **Tests come with the change.** A bug fix carries a test that fails without it.
  Fixtures go through the production write path, not hand-inserted rows.
- **CI is green.** Lint, unit and integration tests, cross-compilation and the web
  checks all run on the pull request.

### Coding standards

These are the rules CI enforces; a pull request that breaks one does not merge.

**Go** (`go/`)

- [golangci-lint](https://golangci-lint.run) with the repository's
  [`.golangci.yml`](.golangci.yml) — the CI `lint` job and the pre-commit hook run the
  same configuration. It enables errcheck, staticcheck, gosec, govet, ineffassign,
  cyclop (cyclomatic complexity at most 25), nilerr, errorlint, godot (comments end
  with a period), depguard (package layering — for example, only `cmd/**`,
  `internal/handler`, `internal/events`, `internal/settings` and `internal/config`
  may import `internal/config`), forbidigo and unused. Integration-tagged files are
  linted too.
- No dead code: `bash deadcode.sh` fails on unreachable functions. Keeping one means
  a line with a reason in `go/deadcode-allow.txt`
  ([details](docs/development.md#testing)).
- Platform-specific code lives in `//go:build unix` / `//go:build windows` file
  pairs, not behind runtime checks, and must cross-compile for every release target
  ([details](docs/development.md#platforms)).
- Format with `gofmt`. CI does not gate formatting, so keep unrelated reformatting
  out of the diff.

**Web UI** (`go/web/`, Svelte 5 + TypeScript)

- `bun run check` passes: stylelint with [`.stylelintrc.mjs`](go/web/.stylelintrc.mjs)
  plus the inline-style gate, svelte-check and tsc.
- `bun run test`, the Playwright end-to-end suite and the bundle byte budget pass.
- Visual baseline changes are deliberate: a commit that touches
  `go/web/e2e/__screenshots__/` carries a `[baseline]` marker in its message
  ([visual baseline governance](docs/development.md#visual-baseline-governance-web-e2e)).

### What happens after you open one

The maintainer reviews every pull request, often with AI agents doing a first pass
(see [Built with AI agents](README.md#built-with-ai-agents)). Review comments may come
as commits pushed onto your branch — hardening, tests, documentation — instead of
change requests; that is why maintainer edits should stay enabled. There is no
guaranteed response time.

## License

ctx is licensed under the [Mozilla Public License 2.0](LICENSE). By contributing you
agree that your contribution is licensed under the same terms.
