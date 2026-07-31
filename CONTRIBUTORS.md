# Contributors

ctx is built and maintained by the people listed below. Thanks to everyone
who has reported bugs, audited code, or sent patches.

## Maintainer

- **GottZ** ([@GottZ](https://github.com/GottZ)) — author, architecture, implementation.
  <hire@gottz.de>

## Bug Reports & External Audits

External reports — typically filed by Claude instances running ctx in someone
else's environment — that led to a fix landing in the repository.

- **Damien Moon** ([@DamieMoon](https://github.com/DamieMoon)) —
  [#3](https://github.com/GottZ/ctx/issues/3): root-cause analysis of the guard
  scheduler's `42P08` failure (`jsonb_build_object` with untyped bind
  parameters under pgx extended protocol on PostgreSQL 18). Reproduction,
  diagnosis, fix proposal, and in-repo precedent identification all delivered
  in one issue — went straight to the patch.
- **Damien Moon** ([@DamieMoon](https://github.com/DamieMoon)) —
  [#4](https://github.com/GottZ/ctx/issues/4): root-cause analysis of the
  digest scheduler's `22021` failure (byte-slice title truncation in
  `RunDigest` splits a multi-byte UTF-8 rune, leaving a dangling lead byte
  that PostgreSQL rejects). Standalone deterministic Go reproduction, live
  daemon symptom capture, fix diff, and identification of four latent sites
  with the same byte-slice idiom — all in one issue.
- **Damien Moon** ([@DamieMoon](https://github.com/DamieMoon)) —
  [#5](https://github.com/GottZ/ctx/issues/5) / PR
  [#6](https://github.com/GottZ/ctx/pull/6): `ctx init` wrote non-functional
  Claude Code hooks — the legacy flat `{type,command}` shape (silently ignored
  by current Claude Code, which requires the nested matcher-group form) plus a
  hardcoded `~/.claude/settings.json` that ignored `CLAUDE_CONFIG_DIR`. A/B/A
  probe-script reproduction isolating schema-vs-path, root-cause to the exact
  functions, and a verified fix delivered as a pull request.
- **TurgutKural** ([@TurgutKural](https://github.com/TurgutKural)) — PR
  [#9](https://github.com/GottZ/ctx/pull/9): `/health` reported every cloud
  inference backend as down. The probe demanded HTTP 200 on `GET /`, which only
  local servers (llama.cpp, Ollama) answer — cloud APIs route nothing at their
  root and reply 404/405, so a perfectly healthy backend read as unreachable.
  Diagnosis and a pull request; the shipped fix keeps the reachability
  semantics he identified (any response below 500) and drops the credentials
  from the probe. Also PR
  [#10](https://github.com/GottZ/ctx/pull/10): `dream.language`, a hot-mutable
  server setting that makes the daily-synthesis report language configurable
  instead of hardcoded.

## How to be listed

Open an issue, send a PR, or otherwise help land a change. We add you here
when your work is merged. Use a `Reported-by:` / `Co-Authored-By:` trailer in
the commit if you want a specific name/email recorded.
