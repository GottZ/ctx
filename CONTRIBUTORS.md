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
  instead of hardcoded. Also PR
  [#11](https://github.com/GottZ/ctx/pull/11): cloud relays answer the dream
  link prompt in a named-wrapper drift form (`{"analysis": [...]}`) that the
  parser rejected; the shipped fix keeps his parser-side solution and leaves
  the wire format untouched. Also PR
  [#12](https://github.com/GottZ/ctx/pull/12): a fifth link-response drift
  form — deepseek-v4-flash collapses the array into a bare `{"<uuid>":
  "<type>"}` string map with no confidence field, losing every link in the
  cycle; the shipped fix keeps his recover-the-type approach and floor
  doctrine, hardened with entry discrimination so prose envelopes still
  surface as retryable parse errors. Also PR
  [#13](https://github.com/GottZ/ctx/pull/13): Voyage AI rerankers answer in
  their OpenAPI dialect (`data` container, `usage.total_tokens`), which the
  cohere-shaped client decoded to zero results — every Voyage call failed
  into the un-reranked fail-open order; the shipped fix keeps his
  prefer-results-fall-back-to-data approach, hardened with a probability→
  logit mapping at the wire boundary so Voyage's calibrated [0,1] scores
  survive the downstream sigmoid+blend unmangled. Also PR
  [#14](https://github.com/GottZ/ctx/pull/14): the embed twin of the same
  dialect — Voyage reports `usage.total_tokens` on `/v1/embeddings`, so
  every Voyage embed call went unmetered; the shipped fix keeps his
  prefer-prompt_tokens fallback, hardened with metering visibility and a
  discriminating precedence fixture. Also
  [#22](https://github.com/GottZ/ctx/issues/22): `sensitivity.Scan` flagged
  hash-labelled SHA-256 integrity hashes as `credentials` — the 64-hex rule's
  comment promised a surrounding-context check that did not exist in code,
  silently blocking release-note blocks from every `trust=public` backend;
  plus the `<descriptive placeholder>` assignment-value FP in the same file.
  The shipped fix implements his option 1 (hash-label context discriminator,
  documented as the safer choice over entropy gating in his own analysis)
  and extends the placeholder shapes with angle-bracket templates. Also PR
  [#24](https://github.com/GottZ/ctx/pull/24): a named wrapper holding only
  an empty link array (`{"classifications": []}`, deepseek-v4-flash via
  opencode.ai) hard-errored in the dream parser and burned a transient retry
  on a shape the model reproduces unchanged; the shipped fix keeps his
  "lone empty array = explicit no-links verdict" rule, hardened to decide the
  emptiness structurally (pretty-printed `[ ]` variants), to count keys on
  the raw text (duplicate keys cannot fake a lone array) and to accept the
  canonical prose-plus-empty-array relay shape while two-array wrappers keep
  retrying. Also PR [#25](https://github.com/GottZ/ctx/pull/25): the
  object-map drift form (qwen3.8) repeats the uuid as key and `target_id`,
  so five links overran the 400-token dream-eval cap and the whole evaluation
  was lost; the shipped fix keeps his 600 (re-measured with the Qwen3
  tokenizer at ≈1.5× the array form), turns the exact pin into a lower-bound
  guard and adds `cap_hit` / `links_parsed` llmlog telemetry so the next
  cap-truncation is countable instead of invisible.
- **DojoGenesis** ([@DojoGenesis](https://github.com/DojoGenesis)) —
  [#16](https://github.com/GottZ/ctx/issues/16) / PR
  [#17](https://github.com/GottZ/ctx/pull/17): first boot on a fresh database
  never seeded the backend pool — migration 062's UNIQUE swap
  (`uq_backends_name` → `uq_backends_scope_name`) left the bootstrap INSERT's
  `ON CONFLICT (name)` without an arbiter (SQLSTATE 42P10), so a brand-new
  install came up with an empty pool and a permanent 503 while populated
  deployments never hit the path. Deterministic reproduction, root-cause to
  the exact line, and a one-line fix with an integration regression test
  (red on the old code with the exact live SQLSTATE, green on the fix)
  delivered as a pull request. Also filed the fresh-install audit trio
  [#18](https://github.com/GottZ/ctx/issues/18) (bootstrap admin key missing
  from the compose environment block — G17 compose-gap class),
  [#19](https://github.com/GottZ/ctx/issues/19) (ops scripts assume the home
  deployment: container name, absolute paths, host CLI, corpus shape) and
  [#20](https://github.com/GottZ/ctx/issues/20) (pre-commit hook unparseable
  by stock macOS bash 3.2 — case arm inside `$( )`), each verified against
  the shipped artifacts. The #20 report's suggested grep rewrite shipped as
  the fix (v4.25.2), equivalence-proven against the old globs under real
  bash 3.2; the #18 report's compose declaration + first-run docs shipped in
  v4.25.3, and its G17-class framing directly surfaced the next instance
  (`CTX_SETTINGS_DISABLE`), closed in the same release; the #19 report's
  resolution order (env → compose-derived → literal) shipped in v4.25.5
  across all six ops scripts, together with its corpus-threshold SKIP split
  that turns test.sh into a fresh-install acceptance suite.
- **TresPies-source** ([@TresPies-source](https://github.com/TresPies-source)) —
  [#21](https://github.com/GottZ/ctx/issues/21): a `/v1`-suffixed
  `base_url` — the root shape every other OpenAI-compatible integration
  teaches — passed `backend-create` validation silently and then failed in
  three disconnected-looking ways (probe "unreachable", window discovery
  404 → fail-closed synthesis naming neither backend nor URL, chat 404),
  with a deterministic reproduction chaining all three symptoms to the one
  URL-shape slip. The shipped fix (v4.25.4) rejects a versioned root at
  create/update with a 422 naming the convention, for every openai- and
  rerank-protocol backend — the wire-path sweep the report prompted showed
  the doubling is not openrouter-specific.
- **derLonius** ([@W4-NERF](https://github.com/W4-NERF)) — PR
  [#23](https://github.com/GottZ/ctx/pull/23): the dream-temporal Phase-2 LLM
  review ran under a hardcoded 90 s ceiling that slow reasoning models
  (nemotron-super-trt, ~92 s on full prompts) exceed, silently dropping the
  LLM-found extra dates for that block version; the shipped feature keeps his
  hot-reloadable `dream.temporal_timeout` key with its byte-identical default,
  hardened with the `timeouts.dream` row-precedence documentation, a V16 sign
  check, a V16b cycle-budget warning and a wire-level test that pins both the
  call site and the precedence.

## How to be listed

Open an issue, send a PR, or otherwise help land a change. We add you here
when your work is merged. Use a `Reported-by:` / `Co-Authored-By:` trailer in
the commit if you want a specific name/email recorded.
