# Development

## Building

```bash
go build -o ctx ./cmd/ctx/           # CLI
go build -o ctxd ./cmd/ctxd/         # Daemon
go test ./... -short                  # Unit tests
```

The CLI (`cmd/ctx`) never depends on the frontend. Plain `go build` / `go install .../cmd/ctxd` need no Bun and produce a binary that serves a 503 placeholder instead of the UI — `docker compose build ctx` is the channel that ships the real UI.

## Web UI (Svelte 5 + TypeScript + Vite, Bun)

The admin SPA lives in `go/web/` and is embedded into the ctxd binary via `go:embed`. The Docker image builds it in its own stage (`oven/bun:1.3-alpine`, `bun install --frozen-lockfile`, `svelte-check` gate).

```bash
cd go/web
bun install                           # once; bun.lock is committed
bun run dev                           # Vite on :5173, proxies /api → ctxd
bun run check && bun run build        # typecheck + production build into dist/
```

The dev proxy targets `http://localhost:8080`; the compose ctx service publishes no ports by default — add a local port mapping (see `docker-compose.override.yml.example`) and override with `CTX_DEV_PROXY=http://127.0.0.1:<port>` if you map a different port.

### Shell & theming

All surfaces theme from one token set (Tokyo-Night-adjacent dark + a light counterpart) via a `data-theme` attribute on `<html>`. The preference (`system`/`light`/`dark`) is detected from `prefers-color-scheme`, follows the OS by default, and persists in `localStorage`; a render-blocking, `script-src 'self'`-compliant boot script (`/theme-boot.js`) applies it before first paint (no dark-flash). A three-segment toggle in the nav-rail footer switches it.

The shell is a collapsible left **nav rail** (icon-only on narrow desktops, an off-canvas drawer with focus-trap on mobile). Rail entries are **role-adaptive** — filtered from the caller's `whoami` capabilities, so a member sees the corpus areas while admins additionally get tenant/server sections. The rail footer carries an **identity badge** (API-key label, owning tenant, role badge owner/admin/member, read-only marker when the key has no writable scope). Each area declares a layout mode: reading surfaces (Settings/Status) stay centered at a readable measure, canvas/master-detail areas (Graph/Blocks/Chat) use the full viewport width. Empty results render a guiding empty state with an onboarding CTA. `/` lands each tier on its home area — members on a `/home` capability screen, higher tiers on the status dashboard — and a client-side tier guard redirects a forbidden deep link back there (visibility only; the real authorization stays server-side).

### Typography & design system

One design-token set (`go/web/tokens/tokens.json` → generated `src/styles/tokens.css`, drift-gated) drives every surface; a family-scoped Stylelint ratchet forbids off-token colour/box/motion/typography literals. The aesthetic direction is **"instrument panel with its own voice"**: a self-hosted characterful monospace carries the wordmark, headings and number-dense surfaces (status tiles, counts, table figures, code), while the body stays a quiet system stack.

The mono is **JetBrains Mono v2.304** (SIL Open Font License 1.1; the licence ships next to the font at `go/web/public/fonts/OFL.txt`, as the OFL requires), self-hosted as **one variable woff2** (weight axis 100–800, ~111 KB — covers the 400/500/600 weights in use from real glyphs, no font-synthesis). It is declared `@font-face { font-display: block }` in `app.css`, preloaded in `index.html`, and served same-origin so the CSP `font-src 'self'` covers it with no external request. Applied purely through the `--font-mono` token (which now lists `'JetBrains Mono'` first), so every existing mono surface inherits the character with no per-component change. Bundling the font also makes UI-text rendering deterministic across environments — it removes the system-mono variance the old `ui-monospace` fallback stack carried, which is why the visual baselines only ever needed the container before.

**Motion & the signature element.** All transitions run on the motion tokens (`--dur-1`/`--dur-2`/`--ease`); the three long component animations (login rise, chat streaming cursor, wordmark pulse) stay component-local but are switch-off-able. A single global `@media (prefers-reduced-motion: reduce)` guard in `app.css` zeros every animation- and transition-duration via `*` — one place, no per-component opt-in needed. Following the "spend your boldness in one place" rule the UI carries exactly **one** signature element: the wordmark's blinking terminal cursor (`Wordmark.svelte`), a decorative pulse marked `aria-hidden` so it never enters the accessible link name (which stays a deterministic `"ctx"`). That abschaltbarkeit is a gate, not a promise: a **reduced-motion walk** (`go/web/e2e/contract/reduced-motion.ts`, driven by `reduced-motion.spec.ts`) emulates `prefers-reduced-motion: reduce`, sweeps every element and asserts none carries a running duration — with the signature asserted silenceable by name. Removing the global guard turns it red (the button/input transitions depend on it alone). A signature animation is otherwise held to the same 0-pixel visual gate as everything else: it must render its captured (`animations:'disabled'`) frame identically and must not add per-frame repaint load that jitters concurrent captures — the reason the cursor stays a discrete two-step `visibility` blink rather than a continuous opacity pulse.

### Areas

- **Settings** — renders the full [Settings API](api.md#settings-api) catalog generically from registry metadata: one category card per key prefix, widgets dispatched by registry type (an unknown future type degrades to read-only), source badge (`default`/`env`/`db`) and env-var name per field. Hot and `coupled:embed-cache` keys edit live (one `PUT` per changed key, a `422` lands inline); restart/coupled keys render read-only. Fields with a `db` override get a reset affordance. The three cross-field rules (thresholds, dual-runner `num_ctx`, `blend_weight`×graph) are mirrored client-side as inline previews while the server-side candidate build stays authoritative. Includes the **Backends** sub-route (`/settings/backends`) — backend-pool editor with a trust dropdown + elevation-confirm dialog, roles multi-select, model_map line editor, priority up/down and per-row reachability test — plus the write-only **secrets vault** with reference tracking.
- **Graph** (`/graph?focus=<uuid>`, deep-linkable) — renders dream-link ego networks via sigma (WebGL) over one graphology multigraph instance (dream and structural edges can share a directed pair; the client already consumes the additive `structural_edges`/`struct_rels`/`origins` wire fields tolerantly and shows them default-visible — server-side delivery lands with the graph-structural backend waves) (deliberately outside Svelte reactivity — the runes proxy overhead on thousands of node objects is the documented reason). Entry is the FTS search; a hit/node click focuses that block's ego net (`GET /api/graph/ego`, 2 hops). Double-clicking a node expands it (+1 hop merge); the layout is ForceAtlas2 in a web worker (Blob-URL — CSP carries `worker-src blob:`), running 3–10s scaled by graph size after every merge. Client memory is hard-capped: over 5000 nodes / 20000 edges the nodes farthest from focus (BFS distance, LRU tie-break) are evicted down to 4000 — pinned nodes and focus survive. One filter state (link class, min confidence, category, created window) drives both sides: loaded elements filter instantly through the sigma reducers (zero server roundtrips) while new fetches mirror the filters as ego-query params. The link-class (dream + structural) and category filters render as compact multi-select dropdowns — all/none quick actions, per-row `only` isolation, the trigger summarising the selection (accent-marked when filtering); the link-class list doubles as the edge legend (swatches mirror the canvas form language), and the category default (empty allowlist = everything visible) presents as all-checked. A `labels` toggle in the meta-row hides the canvas node labels for dense graphs (display option, survives a filter reset; hover boxes and open-window highlights keep their labels — they render through sigma's separate hover layer). Single-click opens the node's floating detail window (content lazy through scope-checked `manage get`; content renders as a text node, never `{@html}`); a search-hit pick centres AND opens it through the same path. Shift+mouse-wheel rotates the camera (two-finger touch rotation is native sigma); plain wheel keeps zooming.
  **Pluggable renderers (RV1):** the ego view can render through four engines over the SAME graphology instance, switched live from a meta-row select (choice persists in localStorage; default stays sigma). Each renderer is a lazily imported Svelte component behind a shared contract (`src/lib/graph/renderers.ts`: props + `resetCamera()`/`refresh()`, capability flags `labels`/`ownsLayout`/`threeD`); the buffer renderers consume `src/lib/graph/render-buffers.ts` (graphology → typed arrays with baked colors, filter visibility incl. the hidden-endpoint rule, and `hop`/`createdAt` as semantic z sources) and track FA2 layout motion through graphology attribute events (rAF-throttled). The engines: **sigma** (default — canvas labels, hover layer, the e2e semantics hook), **cosmos** (`@cosmos.gl/graph`, GPU force layout + rendering; it owns the layout, so the page pauses the FA2 worker while active and positions never write back to graphology), **deck** (deck.gl Orthographic 2D with an optional Orbit 3D toggle — z is semantic: hop rings step downwards), and **three** (three.js 2.5D — instanced spheres + line segments, an in-canvas `z:` select maps depth to hop distance, created-at time, or flat). Non-sigma renderers show labels only as hover tooltips (the `labels` toggle disables itself via the capability flag). Purpose: an in-product shootout of the 2026-08 graph-viz research shortlist on real corpora before committing to a successor stack; the renderer chunks load only when selected (the first-load budget is untouched).
- **Blocks** — corpus browser: full-text search + category/tag/scope facets + a sensitivity-badged, keyset-paginated newest-first list over the scope-gated `/api/search`, a detail panel, and create/edit/delete over `/api/store` + `manage update`/`delete` (sensitivity-downgrade + delete confirms).
- **Status** dashboard + SSE live stream.
- **Chat** (`/chat`) — streams a turn from [`POST /api/chat/stream`](api.md#web-chat-sessions) over fetch + `eventsource-parser` (no reconnect — a turn is one-shot). The thread shows the user message, collapsible tool-call cards, the streamed assistant answer and a backend badge. Assistant markdown goes through **markdown-it `html:false` + DOMPurify**, with `[title](ctx:<id>)` citations rewritten to `/graph?focus=<id>` BEFORE sanitizing (raw HTML in a quoted block is escaped, never parsed; `markdown.ts` carries the XSS suite). A turn is abortable and aborts on navigate-away/`beforeunload` (frees the single llama.cpp slot).
- **Admin / tenant areas** — the server-admin **tenant register** (`/admin`), tenant detail pages with per-scope quota forms plus a **cross-tenant read-grants** panel (list/create/revoke the scope grants where the tenant is the grantee), and the tenant-admin **keys** area (`/tenant`) that lists/creates/revokes keys (show-once plaintext reveal, self-revoke guard) with a quota card and scope self-provisioning. Server-admins also drive a guided **project-provisioning wizard** from `/admin` that sequences the existing compound over three steps — the atomic `tenant-create` (tenant + main scope + owner key), a repo `scope-create`, and a K12 **agent-key** mint (home = the repo scope, `allowed_scopes=[]`, `write_scopes=[]`) — with an alternative entry that adds a repo scope + agent key to an existing tenant (two calls); each key is revealed once and the flow resumes mid-way after an abort (the repo scope already exists). The tenant-admin **backend pool** (`/tenant/backends`, inherits the `/tenant` tenant-admin guard) reuses the settings backend-pool editor in a tenant-scoped variant (pool only; the secrets vault stays server-admin). See [multi-tenancy](multi-tenancy.md#self-service-onboarding-v411). Server-admins also manage the **block-type registry** (`/admin/types`, inherits the `/admin` server-admin guard) — a declarative form over each type's classification policy (retrieval/guard/dream/structural-parent) built on the shared table + modal primitives; builtin `_global` types are delete-protected (control disabled UI-side, server 409 is the gate) and a server-side 422 keeps the form open with the input intact (draft-preserving field errors).
- **Workflow** (`/issues`, `/issues/:id`, `/board`) — the project issue surface, **GA since v4.3.0** (the `Workflow` rail section and the `/home` tile show once `whoami.capabilities.workflow` is true — statically true today; a later per-tenant gate changes only the value, not the read site). `/issues` is a **virtualized** issue list (hand-rolled fixed-height DOM windowing over the shared Table primitive, <200 live rows at the 50k keyset cap) with a filter bar (workflow-status / labels / full-text `q`), a 0/1/N project picker and **lossless URL state** (filters survive reload and deep-link); browse mode keyset-appends while search shows a Top-N with the load-more affordance structurally suppressed. It is read-only — every write lives in the detail. `/issues/:id` is the full issue: the markdown body and every comment render through the shared sanitized markdown path (markdown-it `html:false` + DOMPurify, the same single sink as chat), a comment composer, a status-transition confirm dialog (a `422` stays visible and keeps the selection), and title edit — all gated by a **fail-closed** writable derivation (a read-only banner when the scope is not writable); plus a sync-state badge (5 known verdicts + an unknown fallback) and a uniform `404` → empty state (no redirect loop). `/board` is a **read-only kanban** over the same `…/board` wire the CLI [`ctx kanban`](cli.md#board) eats: the status columns come straight from the wire in wire order (== the type-config status order, never hardcoded), each with its wire count; the open/closed/unmapped verdict is joined from the registry (`GET /api/types` → `config.workflow.terminal` — the board wire carries no category), so terminal columns start **collapsed** (count visible, expandable) and a status the registry does not know renders as a read-only `unmapped` column instead of being dropped. Each column keyset-windows its own cards (per-column resume cursor; the count is the wire aggregate, so a column shows "30 of 10 000" with a bounded DOM). A **writable** board (fail-closed: the caller may write the project scope) turns each card into a drag source and each column into a drop target — a drop is an **optimistic** status transition (the card moves at once, then a `PATCH /api/project/{id}/issues/{block_id}` confirms; a `409`/`422` rolls the card back and re-reads the board wire so it reflects the registry truth), with a full mouse-free path (a per-card Move dialog that picks the target column). A read-only board keeps the U07 render (no drop targets, no Move affordance). The shell runs `/board` in a `board` layout mode (full-bleed + horizontal scroll) and reserves the `/webhooks` server prefix for the forge-sync receiver (W13). **Live updates** ride the member SSE domain-event stream ([`GET /api/project/events`](api.md), Achse-03-W9 — ids-only frames, scope-filtered per key) through a `LiveSource` (`go/web/src/lib/workflow/live.ts`) that debounces a frame burst into one refetch (a targeted-id frame reloads the affected list head / the viewed issue; an `issues-bulk` or `resync` frame reloads once, never count-many); the 10 s visibility-paused **poll stays the permanent fallback** whenever the stream is not open (429 cap / abort / revoke). SSE reuses the Bearer-capable `SseClient` (fetch + `eventsource-parser`), not a native `EventSource` (which cannot carry the `Authorization` header). On mobile (< SM) the detail renders as a full-bleed G6 sheet and the board becomes a single-column pager (U09); on desktop a board card opens the issue in a floating window (shared `lib/windows`).

## Testing

```bash
bash state.sh                       # Live system state
bash test.sh --with-ollama          # 18 system + retrieval + MCP tests
bash eval.sh                        # 43 eval tests (baseline regression)
bash eval.sh --update-baseline      # Set a new baseline
cd go && go test ./... -short       # Go unit tests
```

MCP tool handlers return `Content[].text` (no structured output) — tested in `test.sh` T17/T18.

### Model benchmark: ctx-goldbench

`ctx-goldbench` measures how well an arbitrary OpenAI-compatible model performs ctx's
LLM tasks — it replays the real pipeline system prompts (exported via per-package
`bench_exports.go` accessors, no behavioural change) against a candidate model, parses
the output with the real ctx parsers, and scores against 1127 anonymized gold cases from
the [ctx-bench](https://github.com/GottZ/ctx-bench) dataset repository (12 axes:
temporal extraction on block and query level,
keywords, tagging, title, dream links, recurrence, sensitivity classification,
cluster labeling, rerank judging, synthesis contract, query translation).

```bash
cd go && go build ./cmd/ctx-goldbench
./ctx-goldbench -data ../bench/goldbench/data \
  -endpoint http://localhost:11434/v1 -model <model> -axes all \
  -out report.json -md report.md      # -dry-run validates the dataset without HTTP
```

`parse_rate` is a first-class metric on every axis: a model that cannot serve ctx's
output contracts is unfit for ctx regardless of content quality. Since metric v2
(`metric_version` in the report env stamp) the keyword/tagging axes score a
prediction-capped set-F1 (over-generation no longer inflates recall), cluster
labeling scores a constraint-gated token-F1, every axis carries a bootstrap 95%
confidence interval, and reports include token throughput (prompt/completion
tok/s) plus a `server_note` provenance field for the serving flags. Reports also
carry `fail_stats` (top-level and per axis): `context_errors` counts cases the
server rejected at its context limit, `truncated_outputs` counts cases whose
completion hit the max_tokens budget (`finish_reason: length`), and
`transport_errors` the remaining call failures — so a score drop caused by a
serving limit is distinguishable from genuine model weakness. For reasoning
models whose thinking tokens consume the answer budget, `-max-tokens-mult N`
scales every axis budget (stamped as `max_tokens_mult` in the env; such runs
are only comparable to runs with the same multiplier — the unscaled run stays
the ctx-fitness verdict). `-extra-body '<json>'` merges a JSON object into
every chat request (struct fields win) for engine-specific knobs the portable
API cannot carry — thinking switches via `chat_template_kwargs`, `top_k` or
penalty samplers on vLLM; the object is stamped into the report env as
`extra_body`. `-temperature-override N` replaces the per-axis pipeline
temperatures with a fixed value (stamped as `temp_override`) — a declared
mock-fidelity deviation for "model-card-pure" runs that measure the model
under its recommended samplers instead of the ctx pipeline contracts.
Thinking models are carried structurally: `<think>…</think>` blocks are
stripped client-side before parsing (counted as `think_stripped` per axis and
in `fail_stats` — no axis contract legitimately contains think tags), and
`usage.completion_tokens_details.reasoning_tokens` is aggregated into the
throughput block as `reasoning_tokens` where the server reports it, so the
reasoning tax is measured instead of inferred from truncations. Dataset card,
axis table and anonymization method: [github.com/GottZ/ctx-bench](https://github.com/GottZ/ctx-bench).

### Wire-contract freeze (workflow UI)

The workflow-UI API client (`go/web/src/lib/api/issues.ts`, `types-registry.ts`) and the SPA e2e/vitest fixtures both eat the **same** contract-freeze JSONs in `go/web/src/lib/api/__fixtures__/*.json` (issue list/detail/comments/board/mutate, project list, sync status, type list). Those files are re-serialized from the live handler structs (W6/W7/W11/W4/types) by the Go golden `TestContractFreezeGolden` (`internal/handler/contract_freeze_golden_test.go`) — a drift on either side turns it red before deploy (closes the fixture-drift gap: the FE mocks can no longer diverge silently from the Go wire). To regenerate the JSONs after an intentional wire change, review the diff from:

```bash
cd go && UPDATE_FREEZE=1 go test ./internal/handler -run TestContractFreezeGolden
```

The path prefix of the whole workflow surface lives in exactly one constant each (`ISSUES_BASE` = `/api/project`, `TYPES_BASE` = `/api/types`); the client functions and the e2e fixture namespace matcher (`go/web/e2e/issue-fixtures.ts`) both import it, so an un-mocked path inside the namespace hard-fails loudly (599) instead of a benign `{success:true}`.

### Live tier (PV10) — real ctxd, production write paths

Beside the browser-mocked suite there is a second, small tier (`go/web/e2e/live/`, ≤ 15 `@live` specs) that runs against a **throwaway** ctxd + Postgres brought up per run by `docker-compose.e2e.yml` (`bun run e2e:live` → `run-live.sh`). It proves the classes the mock tier cannot: real server **enforcement** (tenant isolation with a positive control), fixture-**shape** truth (W10), and real **SSE transport**. The corpus is seeded **only through production write paths** (`tenant-create` → `store` → `issue-create`), so the seed itself is an integration test of those handlers and no hand-written shape can drift from the server. A three-layer **fail-closed target gate** (`seed.ts`, design 06 §3.6) refuses to write to anything but the job-local instance (env-gate + host binding, a per-run bootstrap-key handshake via `GET /api/whoami` — the PV10a `CTX_BOOTSTRAP_ADMIN_KEY` mints the first server-admin key only on an empty DB, [operations](operations.md#environment-variables), and the key is valid solely against the instance that dies with the stack). The tier makes **no pixel assertions** — no screenshot baselines here. Runbook, the three negative gates (W10 shape-mutation / target-gate refusal / leak-detector), the trace-secret handling and the **release-gate rule** (a version tag is pushed only after a green nightly `web-live` run — the "CI is truth" rule extended to the live tier, with the staging-redaction caveat) live in [`go/web/e2e/live/README.md`](../go/web/e2e/live/README.md). CI runs it nightly (`schedule`) + on `workflow_dispatch` + on PRs labelled `e2e-live` — never on every PR (the mock `web` job stays the per-PR frontend gate).

## Bundle byte budget (Web, PLc)

The release bundle is guarded by a deterministic on-disk byte gate: `bun run test:budget` (in `go/web/`) reads **every** `dist/**/*.{js,css}` artifact plus its precompressed `.br` sibling — the bytes the Go handler actually serves (verify lens = release lens) — and asserts against `go/web/e2e/perf/chunk-budget.json`: a per-chunk budget for every chunk (named override or per-extension default), a transfer total, and a "no missing `.br` sibling at/above the compression threshold" invariant. Zero runner variance (pure byte counts, no browser). Run it **after a real release build** (`bun run build`, no `VITE_E2E`); a missing/empty `dist/` is a hard red, never a skip.

Budgets were calibrated **inside the digest-pinned toolchain container** (brotli output is toolchain-version-sensitive; the plugin runs at brotli default Q11). Runbook: a `toolchain.lock` digest bump is a **re-baseline event** — re-measure in the new container and update `chunk-budget.json` (budgets + `toolchain_lock_sha256_12`) in the same commit, otherwise the gate goes falsely red after the update. Loosening a budget is a visible decision with a reason in the commit message, never a silent side effect of a feature commit.

## Lighthouse timing trend (Web, nightly — never a gate)

`bun run perf:lhci` (in `go/web/`, config `lighthouserc.cjs`) runs Lighthouse 3× against a statically served release build and writes lhr reports to `e2e/perf/.lhci/`. Role boundary: **bytes are judged by the PLc budget gate above** — LHCI is a lab **timing** probe only, runs nightly in CI (`web-lhci` job, schedule + manual dispatch, never on PRs) and every assertion is warn-only with deliberately uncalibrated thresholds. Lab timings on shared runners carry a **±20–40 % noise floor**: a single run is never signal, only jumps beyond ~40 % across the trend are. If timing budgets are ever introduced, calibrate them from the **median of several nightly runs**, never a single run. Chromium and node come from the pinned toolchain image; verdicts are only comparable in-container.

## Visual baseline governance (Web e2e)

Screenshot baselines (`go/web/e2e/__screenshots__/`) are the frozen "objectively good" reference for the UI: the taste judgement is made once, at baseline approval — afterwards every pixel deviation is a measurable diff, not an opinion. Baselines are only valid when rendered inside the digest-pinned toolchain container (`go/web/e2e/toolchain.lock` pins the image digests; `bash go/web/e2e-visual.sh --update` is the only regeneration path — CI has no update path and only compares).

The comparison runs at `maxDiffPixels: 0` (with a calibrated per-pixel `threshold` for compositing anti-alias jitter) — a deliberately strict global policy. The **only** sanctioned relaxations are per-shot (design 05-§8.E3 / 06 §4.3 escalation ladder: mask → stylePath → per-shot `maxDiffPixels`), never a global loosening. A per-shot budget lives on a `PageState.visualTolerance` (contract registry) with a mandatory reason + issue anchor and is validated structurally. The single current use is the board **empty** state: the self-hosted Mono draws accent-blue glyph edges against the wide `surface-0` field, and under parallel-worker load the pinned SwiftShader rasteriser wobbles one such edge pixel by ±1 LSB (deterministic in isolation, not retry-clearable on that sparse shot). Its `maxDiffPixels: 4` tolerates the measured 1-pixel jitter and stays orders of magnitude below any real regression on that shot.

Any commit that touches `__screenshots__/` or adds a11y-debt entries (`go/web/e2e/a11y-baseline.json`) must carry a **`[baseline]`** marker in its message plus a one-line reason. The `commit-msg` hook rejects it locally (fast feedback), and the CI marker gate rejects it on the PR (enforcement that survives `--no-verify` and dead hooks). Shrinking the a11y debt (ratchet) needs no marker. Baseline changes live in their own commits, never mixed with feature code, and are **batched per wave group** (one consolidated `[baseline]` commit per group, squashed before merge) to keep the non-delta-compressible PNG history rate bounded.

**Playwright/toolchain upgrade runbook** — an image bump and the baseline regeneration are ONE coupled step, never separate:

1. Bump the image digests in `go/web/e2e/toolchain.lock` (the tag next to it is a human-readable label, the digest wins).
2. Regenerate: `bash go/web/e2e-visual.sh --update` (builds the container from the new pins, refreshes changed baselines inside it).
3. Commit lock bump + regenerated baselines together as ONE `[baseline]` commit stating the upgrade as the reason.

## Flake quarantine, budgets & trend (Web e2e, PV11)

Retries are declared infrastructure protection (CI `1`, local `0`), not a flake blanket. Every `flaky` outcome (a test that only passed on retry) becomes a visible CI annotation (`.github/scripts/flake-annotations.sh`) and flows into the nightly trend line. **Process rule: a test flaky twice within 14 days must be quarantined or fixed** — this is not optional (also stated in the `e2e/COVERAGE.md` header).

**Quarantine** is a `@quarantine` tag on the test **plus** an entry in `go/web/e2e/quarantine.json` (`title` + mandatory `issue` + `since`). The per-PR mock-tier gate excludes tagged tests (config `grepInvert`), while the nightly run keeps executing them (`CTX_E2E_QUARANTINE=1`, set on the schedule/dispatch `web`-job step) — quarantine means *observed, not forgotten*. Two gates in `e2e/contract/quarantine.test.ts` keep it honest: a **hard cap** of 5 entries (> 5 ⇒ red), and a **tag↔ledger bijection** (a tag with no entry ⇒ red `untracked quarantine`; an entry with no tag ⇒ red `stale`). The tag set is resolved structurally from `playwright test --list --reporter=json`, not from a source grep. The empty ledger is the healthy default.

**Runtime budgets** (`.github/e2e-budget.json`, calibrated) split the e2e wall time (from `report.json`) from the whole-job wall; over budget annotates, `> 10 min` fails the job. **Three consecutive runs over the e2e part budget trigger sharding** — the blob-reporter foundation is already laid, so activation is a documented YAML diff in `ci.yml` (`vars.E2E_SHARD`), currently dormant (measured e2e ≈ 77 s ≪ the trigger). Nightly, a dedicated `web-trend` job writes one duration+flaky JSON trend line per run (`e2e-trend.sh`, retention 90 d) and measures the cumulative `__screenshots__` git-history blob volume (`history-budget.sh`): it annotates from 60 MB and flags the documented escalation path (baseline orphan branch / Git-LFS) from 150 MB, but never auto-fails — that decision is taken with the measurement, not by the CI run.

## README infographic (docs/how-ctx-works*.svg)

The README architecture infographic is a hand-authored animated SVG in two theme variants, selected by GitHub's `<picture>` / `prefers-color-scheme` mechanism. **Edit only `docs/how-ctx-works.svg`** — all colors live in CSS custom properties inside the `/* THEME-START */ … /* THEME-END */` block, and `docs/how-ctx-works-dark.svg` is that same file with only this block swapped. To regenerate the dark variant after an edit, replace the theme block with the dark palette (GitHub dark colors: bg `#0d1117`, panel `#161b22`, accents `#3fb950`/`#58a6ff`/`#a371f7`/`#d29922`/`#f85149`) and keep everything else byte-identical — a `re.sub` over the marker block, never a hand-edit of both files. Icons are emoji (system font stack incl. Noto/Apple/Segoe emoji), arrow flow is a CSS `stroke-dashoffset` animation with a `prefers-reduced-motion` off-switch. Verify rendering with headless Chromium (`chromium --headless --screenshot=… --window-size=1200,2170 <url>`) — serve the file over HTTP, `file://` is blocked in some tools.

## Git hooks

Enable with `git config core.hooksPath .hooks`:

- **pre-commit** — golangci-lint on staged Go files.
- **commit-msg** — enforces a documentation review for `feat:` commits and schema migrations (`go/migrations/`): the commit must stage `README.md` **or** a `docs/*.md` file. Also enforces the `[baseline]` marker (above).
- **pre-push** — requires annotated `v*` tags for version releases.
