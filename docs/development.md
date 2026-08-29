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

- **Settings** — renders the full [Settings API](api.md#settings-api) catalog generically from registry metadata: one category card per key prefix, widgets dispatched by registry type (an unknown future type degrades to read-only), source badge (`default`/`env`/`db`) and env-var name per field. Hot and `coupled:embed-cache` keys edit live (one `PUT` per changed key, a `422` lands inline); restart/coupled keys render read-only. Fields with a `db` override get a reset affordance. The three cross-field rules (thresholds, dual-runner `num_ctx`, `blend_weight`×graph) are mirrored client-side as inline previews while the server-side candidate build stays authoritative. Includes the **Backends** sub-route (`/settings/backends`) — backend-pool editor with a trust dropdown + elevation-confirm dialog, roles multi-select, model_map line editor with a per-row params JSON editor (free object, validated client-side; any key is meaningful since the generic wire passthrough — `chat_template_kwargs`, `think`, `max_tokens`, provider knobs), priority up/down and per-row reachability test — plus the write-only **secrets vault** with reference tracking.
- **Graph** (`/graph?focus=<uuid>`, deep-linkable) — renders dream-link ego networks via sigma (WebGL) over one graphology multigraph instance (dream and structural edges can share a directed pair; the client already consumes the additive `structural_edges`/`struct_rels`/`origins` wire fields tolerantly and shows them default-visible — server-side delivery lands with the graph-structural backend waves) (deliberately outside Svelte reactivity — the runes proxy overhead on thousands of node objects is the documented reason). Entry is the FTS search; a hit/node click focuses that block's ego net (`GET /api/graph/ego`, 2 hops). Double-clicking a node expands it (+1 hop merge); the layout is ForceAtlas2 in a web worker (Blob-URL — CSP carries `worker-src blob:`), running 3–10s scaled by graph size after every merge. Client memory is hard-capped: over 5000 nodes / 20000 edges the nodes farthest from focus (BFS distance, LRU tie-break) are evicted down to 4000 — pinned nodes and focus survive. One filter state (link class, min confidence, category, created window) drives both sides: loaded elements filter instantly through the sigma reducers (zero server roundtrips) while new fetches mirror the filters as ego-query params. The link-class (dream + structural) and category filters render as compact multi-select dropdowns — all/none quick actions, per-row `only` isolation, the trigger summarising the selection (accent-marked when filtering); the link-class list doubles as the edge legend (swatches mirror the canvas form language), and the category default (empty allowlist = everything visible) presents as all-checked. A `labels` toggle in the meta-row hides the canvas node labels for dense graphs (display option, survives a filter reset; hover boxes and open-window highlights keep their labels — they render through sigma's separate hover layer). Single-click opens the node's floating detail window (content lazy through scope-checked `manage get`; content renders as a text node, never `{@html}`); a search-hit pick centres AND opens it through the same path. Shift+mouse-wheel rotates the camera (two-finger touch rotation is native sigma); plain wheel keeps zooming.
  **Pluggable renderers (RV1):** the ego view can render through four engines over the SAME graphology instance, switched live from a meta-row select (choice persists in localStorage; default stays sigma). Each renderer is a lazily imported Svelte component behind a shared contract (`src/lib/graph/renderers.ts`: props + `resetCamera()`/`refresh()`, capability flags `labels`/`ownsLayout`/`threeD`); the buffer renderers consume `src/lib/graph/render-buffers.ts` (graphology → typed arrays with baked colors, filter visibility incl. the hidden-endpoint rule, and `hop`/`createdAt` as semantic z sources) and track FA2 layout motion through graphology attribute events (rAF-throttled). The engines: **sigma** (default — canvas labels, hover layer, the e2e semantics hook), **cosmos** (`@cosmos.gl/graph`, GPU force layout + rendering; it owns the layout, so the page pauses the FA2 worker while active and positions never write back to graphology), **deck** (deck.gl Orthographic 2D with an optional Orbit 3D toggle — z is semantic: hop rings step downwards), and **three** (three.js 2.5D — instanced spheres + line segments, an in-canvas `z:` select maps depth to hop distance, created-at time, or flat). Non-sigma renderers show labels only as hover tooltips (the `labels` toggle disables itself via the capability flag). Purpose: an in-product shootout of the 2026-08 graph-viz research shortlist on real corpora before committing to a successor stack; the renderer chunks load only when selected (the first-load budget is untouched).
- **Blocks** — corpus browser: full-text search + category/tag/scope facets + a sensitivity-badged, keyset-paginated newest-first list over the scope-gated `/api/search`, a detail panel, and create/edit/delete over `/api/store` + `manage update`/`delete` (sensitivity-downgrade + delete confirms).
- **Status** dashboard + SSE live stream — health, disable-profile quick-toggles, dream queue, backend pool, dispatch and LLM telemetry. The dream tile carries the **re-dream back-off histogram** (the `ctx dream stats` view: policy line + one bar per occupied `dream_eval_count` level with its effective cooldown), fetched from the `dream-stats` manage action and refetched only when `last_cycle_at` moves (throttled — the distribution is an O(n) GROUP BY server-side).
- **Chat** (`/chat`) — streams a turn from [`POST /api/chat/stream`](api.md#web-chat-sessions) over fetch + `eventsource-parser` (no reconnect — a turn is one-shot). The thread shows the user message, collapsible tool-call cards, the streamed assistant answer and a backend badge. Assistant markdown goes through **markdown-it `html:false` + DOMPurify**, with `[title](ctx:<id>)` citations rewritten to `/graph?focus=<id>` BEFORE sanitizing (raw HTML in a quoted block is escaped, never parsed; `markdown.ts` carries the XSS suite). A turn is abortable and aborts on navigate-away/`beforeunload` (frees the single llama.cpp slot).
- **Admin / tenant areas** — the server-admin **tenant register** (`/admin`), tenant detail pages with per-scope quota forms plus a **cross-tenant read-grants** panel (list/create/revoke the scope grants where the tenant is the grantee), and the tenant-admin **keys** area (`/tenant`) that lists/creates/revokes keys (show-once plaintext reveal, self-revoke guard) with a quota card and scope self-provisioning. Server-admins also drive a guided **project-provisioning wizard** from `/admin` that sequences the existing compound over three steps — the atomic `tenant-create` (tenant + main scope + owner key), a repo `scope-create`, and a K12 **agent-key** mint (home = the repo scope, `allowed_scopes=[]`, `write_scopes=[]`) — with an alternative entry that adds a repo scope + agent key to an existing tenant (two calls); each key is revealed once and the flow resumes mid-way after an abort (the repo scope already exists). The tenant-admin **backend pool** (`/tenant/backends`, inherits the `/tenant` tenant-admin guard) reuses the settings backend-pool editor in a tenant-scoped variant (pool only; the secrets vault stays server-admin). See [multi-tenancy](multi-tenancy.md#self-service-onboarding-v411). Server-admins also manage the **block-type registry** (`/admin/types`, inherits the `/admin` server-admin guard) — a declarative form over each type's classification policy (retrieval/guard/dream/structural-parent) built on the shared table + modal primitives; builtin `_global` types are delete-protected (control disabled UI-side, server 409 is the gate) and a server-side 422 keeps the form open with the input intact (draft-preserving field errors).
- **Workflow** (`/issues`, `/issues/:id`, `/board`) — the project issue surface, **GA since v4.3.0** (the `Workflow` rail section and the `/home` tile show once `whoami.capabilities.workflow` is true — statically true today; a later per-tenant gate changes only the value, not the read site). `/issues` is a **virtualized** issue list (hand-rolled fixed-height DOM windowing over the shared Table primitive, <200 live rows at the 50k keyset cap) with a filter bar (workflow-status / labels / full-text `q`), a 0/1/N project picker and **lossless URL state** (filters survive reload and deep-link); browse mode keyset-appends while search shows a Top-N with the load-more affordance structurally suppressed. It is read-only — every write lives in the detail. `/issues/:id` is the full issue: the markdown body and every comment render through the shared sanitized markdown path (markdown-it `html:false` + DOMPurify, the same single sink as chat), a comment composer, a status-transition confirm dialog (a `422` stays visible and keeps the selection), and title edit — all gated by a **fail-closed** writable derivation (a read-only banner when the scope is not writable); plus a sync-state badge (5 known verdicts + an unknown fallback) and a uniform `404` → empty state (no redirect loop). `/board` is a **read-only kanban** over the same `…/board` wire the CLI [`ctx kanban`](cli.md#board) eats: the status columns come straight from the wire in wire order (== the type-config status order, never hardcoded), each with its wire count; the open/closed/unmapped verdict is joined from the registry (`GET /api/types` → `config.workflow.terminal` — the board wire carries no category), so terminal columns start **collapsed** (count visible, expandable) and a status the registry does not know renders as a read-only `unmapped` column instead of being dropped. Each column keyset-windows its own cards (per-column resume cursor; the count is the wire aggregate, so a column shows "30 of 10 000" with a bounded DOM). A **writable** board (fail-closed: the caller may write the project scope) turns each card into a drag source and each column into a drop target — a drop is an **optimistic** status transition (the card moves at once, then a `PATCH /api/project/{id}/issues/{block_id}` confirms; a `409`/`422` rolls the card back and re-reads the board wire so it reflects the registry truth), with a full mouse-free path (a per-card Move dialog that picks the target column). A read-only board keeps the U07 render (no drop targets, no Move affordance). The shell runs `/board` in a `board` layout mode (full-bleed + horizontal scroll) and reserves the `/webhooks` server prefix for the forge-sync receiver (W13). **Live updates** ride the member SSE domain-event stream ([`GET /api/project/events`](api.md), Achse-03-W9 — ids-only frames, scope-filtered per key) through a `LiveSource` (`go/web/src/lib/workflow/live.ts`) that debounces a frame burst into one refetch (a targeted-id frame reloads the affected list head / the viewed issue; an `issues-bulk` or `resync` frame reloads once, never count-many); the 10 s visibility-paused **poll stays the permanent fallback** whenever the stream is not open (429 cap / abort / revoke). SSE reuses the Bearer-capable `SseClient` (fetch + `eventsource-parser`), not a native `EventSource` (which cannot carry the `Authorization` header). On mobile (< SM) the detail renders as a full-bleed G6 sheet and the board becomes a single-column pager (U09); on desktop a board card opens the issue in a floating window (shared `lib/windows`).

## Testing

```bash
bash state.sh                       # Live system state
bash test.sh --with-ollama          # 18 system + retrieval + MCP tests
bash eval.sh                        # 47 eval tests (baseline regression)
bash eval.sh --update-baseline      # Set a new baseline
bash eval.sh --no-warmup            # Skip the unscored warm-up pass (development runs)
cd go && go test ./... -short       # Go unit tests
```

`eval.sh` fires every query **twice**: an unscored warm-up pass, then the scored
pass. That is the old "run it twice, score run 2" rule moved into the script, and
it roughly doubles the runtime (~6-10 min instead of ~3-5). `--no-warmup` gets the
single pass back for quick development runs — expect cold-start noise in it.

The five `R` cases are labelled **search-endpt** in the report because they measure
`/api/search` (`store.SearchBlocks`), **not** `ctx_rrf`. Only the synthesis cases
(`/api/query`) exercise the 4-Way RRF. The JSON keys stay `retrieval` / `R0x` — the
baseline compares on them.

#### What a run records about the corpus

Every result file carries the census of the corpus the run measured against:
`by_block_type` (block count per **retrievable** type) and `population` (their sum,
the number a pass rate is a rate *of*). `total_blocks` alone cannot separate a real
retrieval regression from a corpus that grew a new retrievable type. The census comes
from one extra read — the admin-gated drift section of the `stats` action — taken with
`CTX_ADMIN_KEY` from `.env` if it is set, otherwise with the harness key. It is
optional in every sense: without admin rights `by_block_type` is `null`, the reason
stands in `census_source`, and the eval run is unaffected.

#### `--update-baseline` leaves a record

Before it copies the result file over `.eval-baseline.json`, the script writes
`.eval-baseline.diff` next to it: both run timestamps, the `summary` delta, pass/total
per category, the **list of test cases whose verdict flipped** in either direction, and
the census delta. With no previous baseline the file holds the single line
`no previous baseline`. Moving a gate is allowed; moving it without a trace is not —
paste the diff into the commit that moves the baseline.

#### Test categories

A test ID's first letter selects its category, and the registry that maps the two lives
**once**, in `EVAL_CATEGORIES` at the top of `eval.sh` — the summary table, the results
JSON and the 15-pp per-category regression threshold all read it from there. An ID
whose prefix is not registered counts into no category and is watched by no threshold,
so a new case class means a new registry entry first. `G` (`derived`) is registered and
**deliberately has no cases yet**; the regression check lists it under `EMPTY
CATEGORIES` rather than letting a category with nothing in it read as one that passed.

Both script gates run without a server, without `.env` and without firing a query:

```bash
bash eval-matcher_test.sh           # negative-case matcher contract
bash eval-instrument_test.sh        # census, baseline diff, category registry
```

MCP tool handlers return `Content[].text` (no structured output) — tested in `test.sh` T17/T18.

### Redaction & truncation marker register (`internal/redact`)

Every literal the pipeline writes **into** text it later reads back lives in
`internal/redact`, and nowhere else: `Redacted` (`[redacted]` — the cross-scope
replacement in `store.GuardList`), `Truncated` (`[... truncated]` —
`promptguard.Assemble` plus the synthesis and classify prompt builders) and
`Markers`, the case-folded negative list that `internal/derived` gate G4 rejects
a quote on.

`go test -short ./internal/redact/` walks every non-test `.go` file below `go/`
and goes red on any marker literal outside that package. It is AST-based, so a
marker *named in a comment* is not a finding — only string literals are. The
point is fail-closed-ness of a write-time gate: a writer that spells its own
marker out is a marker the reader does not know, and every quote carrying it
would pass G4 silently. Add a new marker to `redact` first; the register is what
the reader consumes.

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
labeling scores a constraint-gated token-F1 (the production label parser —
and therefore the bench — tolerates one markdown fence wrapping the whole
answer; the structural single-key contract still applies to the unwrapped
body), every axis carries a bootstrap 95%
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

The scoring primitives themselves live in `internal/evalscore` — micro-F1,
token-F1, nDCG@k, the aggregation helpers, the `pg_trgm` similarity
reimplementation and the seeded percentile bootstrap CI — so a second
evaluation harness scores against the same kernels instead of forking them;
`internal/goldbench` delegates to them and keeps only the metrics tied to a
ctx axis contract (the keyword/tagging prediction cap and substring match).
The package also carries the paired statistics an A/B comparison needs:
`McNemar` (exact binomial, the `b/c/discordant/net/p` table) and
`PairedDiffCI` (the bootstrap CI of a per-case difference vector, with the
confidence level as an explicit parameter, so a multiplicity correction
travels with the comparison instead of being baked into the estimator).
Retrieval rankings are scored through `RecallAtK`, `MRRAtK`, `HitAtK` and
`NDCGRanked`, which take an ORDERED LIST OF IDS rather than the score vector
plus positional labels `NDCGBinary` takes — both shapes exist because
converting between them at every call site is where a metric quietly turns into
a different metric. All four return 0 for "no labels" and "no ranking" rather
than NaN: a slice mean is taken over these values, and one NaN would erase a
whole column of a report instead of costing one case.

Two further ranking metrics cover what those four cannot see. In a corpus that
keeps derived blocks (catalogue, insight) next to the sources they were built
from, a derived block can push its own source blocks out of the top-k without
Recall or nDCG moving, because both sides of that trade are relevant.
`SRecallAtK` (subtopic recall) therefore counts covered FACETS instead of
covered documents, and `AlphaNDCG` discounts a facet that an earlier position
already served by `(1−α)` per repeat — `AlphaNDCGDefault` is 0.5, a constant
rather than a config key, because a metric whose parameter moves between two
reports compares nothing. A facet is whatever the caller declares; for ctx it
is a gold block of the source set, and a derived block covers exactly the
facets of its `source_block_ids`. α-nDCG is normalised against a GREEDY ideal
ranking — choosing the true ideal is NP-hard — and the result is deliberately
not clamped to 1, because the greedy ideal is a lower bound and a clamp would
report exactly the case worth looking at as a perfect score. The normaliser
depends on the judgements, k and α only, never on the ranking, so a
base/condition comparison stays sound regardless.

`-dump-outputs <path>` writes every raw model answer as JSONL
(`{axis,id,outputs}`) before any parsing — the substrate for offline
re-scoring (judge-based or retrieval-functional evaluation) without repeating
the serving run. Since dump v2 every line additionally carries the full
prompt transcript and per-request attribution — `system`, `user`, `params`
(the *effective* sampling options after `-max-tokens-mult` /
`-temperature-override`) and `usage` (`prompt`, `completion`, `reasoning`,
`finish`, `think_stripped`, `err`), all indexed in parallel to `outputs`
(one slot per chat request; the sensitivity axis has two) — plus an
optional `gen` engine stamp passed via
`-gen-stamp '{"engine":…,"engine_version":…,"image":…,"template_sha256":…}'`.
`outputs` stays a flat `[]string` (sample 0 per request, post client-side
`<think>` strip), so v1 readers keep working unchanged; the file is created
`0600` because the prompts carry corpus content. The dump path is validated
(opened `O_NOFOLLOW`, chmod `0600`) *before* the serving run starts, so a
symlinked path or a foreign-owned file fails immediately, not after hours of
GPU time.

`-dump-append` (with `-dump-outputs` **and** `-gen-stamp`, both mandatory)
switches the dump from end-of-run to **incremental**: every finished case is
written immediately (mutex-serialized `O_APPEND` line, flushed per case,
`fsync` on close; a case with a transport error or an aborted call is *not*
written), and before the run the existing file is read into a done-set —
cases already present (`axis`,`id`) with a *complete* record (every slot has
an output, no `usage.err`) are neither re-called nor re-written, their
outputs and usage feed the report (scores and fail_stats equal a full run;
`env.resumed_cases` / `env.executed_cases` mark the report, and
`throughput` measures only the executed rest). Resuming after an abort
(Ctrl-C, `kill -9`, ENOSPC) is the identical invocation; a second full run
makes zero calls. Failed records in legacy (end-of-run) dumps do not count as
done — they are re-run and a later complete record wins; two *complete*
records for one `(axis,id)` are refused. Stamp-resume gate: every line of the
existing file must carry the same `gen` stamp as the live `-gen-stamp`
(`engine`, `engine_version`, `image`, `template_sha256`, `model`) — a
mismatch aborts instead of silently mixing two distributions in one corpus
file; put everything that defines the distribution (model/target/quant,
template) into the stamp. One torn last line (abort mid-write, no trailing
newline) is tolerated and truncated before appending; any other parse error,
lines without `axis`/`id`, or a mismatching slot count are refused (the
resume never guesses). The file is `flock`ed exclusively — a second driver on
the same file fails instead of writing duplicates. A sink write error
(ENOSPC/EIO) cancels the run immediately (`ErrDumpWrite`); the current case
is simply re-run on resume. `-dry-run` never writes (and is rejected together
with `-dump-append`). Use this for corpus generation (design 02, KW3); the
plain `-dump-outputs` path is unchanged (truncate + end-of-run write).

`-samples N` (1–64, default 1) asks for N samples per chat request: sample
0 is the unchanged request (client seed — bit-compatible with every previous
run), sample s>0 carries `seed+s` on the wire (`SamplingOpts.Seed`, new);
requests at temperature 0 are issued only once (deterministic — further
samples would be duplicates). `outputs`/`usage` and the whole report stay
sample 0 (a `-samples 8` run is score-identical to `-samples 1`, only the
call count and throughput grow); all samples land in the dump as
`samples`/`samples_usage` matrices `[request][sample]` (including sample 0,
omitted for `-samples 1`). Sampling runs at pipeline temperatures —
`-temperature-override` together with `-samples >1` is refused (a corpus with
a forced temperature is a different distribution); `-samples >1` also
requires `-dump-outputs` (the samples live only there), `-seed >= 0` and no
`seed` key in `-extra-body`. `env.samples` stamps k on the report, `params`
carries the sample-0 seed when k>1, and `fail_stats.sample_errors` counts
cases whose extra sample failed. With `-dump-append` a case is only written
once every sample succeeded (a failed extra sample makes the case incomplete
and it is re-run on resume; remaining slots of such a case are skipped), and
the file must carry the same k as the run (k-gate: a k=3 file resumed with
`-samples 8` aborts instead of mixing).

`-spec-config '<json>'` stamps structured speculative-decoding provenance
into the report env (`env.spec`: `algorithm`, `drafter_path`,
`drafter_sha256`, `drafter_sha_verified`, `gamma`, `engine_build` — an image
*digest*, never a tag —, `target_quant`, `kv_cache_dtype`, `train_step`),
alongside the free-text `server_note`. Unknown fields are rejected. When
`drafter_path` exists locally, goldbench hashes the weights itself
(`model.safetensors`, or all `*.safetensors` shards in sorted order) and
refuses to start on a mismatch with a declared `drafter_sha256` (provenance
conflict — the stale copied hash of an automated training loop); a path that
does not exist keeps the declaration with `drafter_sha_verified: false`, an
existing path without weights is a hard error. Every non-dry run also stamps
`env.concurrency` (the worker count — τ and throughput are regime-dependent).
Reports without the flag carry no `spec`; only dry runs stay fully byte-stable.

`-parse-englog <file>` is a standalone mode (no bench run): it parses a saved
engine stdout log — vLLM (`SpecDecoding metrics:` interval lines + `Avg
generation throughput`) or SGLang (`Decode batch, … accept len`), auto-detected
— and prints a `SpecStats` JSON (`source: "log-parse"`, schema 1) with
normalized τ (tokens per verify step incl. the bonus token) and AR (accepted ÷
drafted draft tokens, no bonus), plus debug fields: `unweighted_line_mean` (the
plain per-line mean used by hand-made tables), `weighted_minus_unweighted`,
`line_tau_min/max`, `steady_decode_tps` (time-weighted over windows with
running requests; windows with `Running: 0` are counted in
`steady_dropped_windows`) and `boot_markers`. vLLM sums absolute interval
counts (AR exact); γ is reconstructed per line as drafted/(accepted/(MAL−1))
and, when consistent across the log, reported as `gamma` — then τ is exact
(`1 + γ·ΣAccepted/ΣDrafted`); otherwise τ falls back to reconstructed verify
steps and is flagged `tau_derived_drafts`. SGLang only logs ratios, so τ is
weighted by `gen throughput × Δt` (`tau_time_weighted`; Δt from timestamps,
capped at 3× the median cadence so pauses do not weigh, default 10 s) and AR
is a lower bound (`ar_lower_bound`, the denominator assumes a full γ per
round). A log with decode windows but no speculative lines (a no-spec
baseline) parses with exit `0` and `no_spec_windows: true`. Exit `2` when the
log has neither spec nor decode windows (boot-only capture), `3` when it
carries more than one engine boot (counted as max of init/start signatures —
windows not attributable), partially drifted spec lines
(`unparsed_spec_lines`), or an unknown/mixed format. The JSON is printed even
on failure with an `error` field. Tail-captured logs (`docker logs | tail`)
carry `boot_markers: 0`.

Prompt changes to the production pipelines travel through the bench first:
a candidate wording runs as an additive `-v2` variant axis side by side with
the baseline (same model, same server, same run — the fairest A/B), and only
a variant with measured benefit on format-breaking models and no regression
on strong ones is promoted into the production prompt. The variant axis is
then retired, since the base axis replays the production prompt and would
double the change. Promoted hardenings so far: the rerank count-match sentence
and the cluster-label single-key sentence (A/B 2026-08-15).

### Retrieval gold set: ctx-goldset

`ctx-goldset` builds the query sets that the retrieval-weight measurement scores
against. It is a sibling of `ctx-goldbench` and shares its determinism
discipline (fixed default seed, provenance stamp), but it reads the live store
instead of a dataset repo. Seven slices, reported separately and never pooled —
the known-item slice is structurally trigram-friendly, so a mean across slices
would transfer a figure between instruments that do not share one:

```bash
cd go && go build ./cmd/ctx-goldset
./ctx-goldset ki     -n 300                 # query = paraphrased title, gold = that block
./ctx-goldset q      -n 225 -concurrency 2  # content-derived questions, on-prem generator
./ctx-goldset qfinal -n 200 -drop 12,77     # hand-check rejects out, seeded DERIV/HOLD split
./ctx-goldset real   -n 150                 # real access-log queries, redaction sweep
./ctx-goldset sess        -n 120            # session windows, gold = daily reports + window blocks
./ctx-goldset mh          -n 100            # dream-link bridges at confidence >= 0.7, gold = both ends
./ctx-goldset glob        -n 80             # aggregating tag questions, gold judged later
./ctx-goldset glob-konstr -n 50             # the same question family with cluster gold — FLOOR CHECK
./ctx-goldset pool   -control 5             # blind pooled judgement template for G-REAL (stage 2)
./ctx-goldset judge  -llm   -template judge-<run>.jsonl        # machine verdicts on-prem, resumable
./ctx-goldset judge  -kappa -controls judged-<run>-controls.jsonl -kappa-min 0.6
./ctx-goldset judge  -draw  -judged judged-<run>.jsonl -draw-seed <seed>  # stratified draw + blind sheet
./ctx-goldset judge  -gold  -sheet fable-sheet-<run>-filled.jsonl -judged judged-<run>.jsonl
./ctx-goldset judge  -calibrate -sheet fable-sheet-<run>.jsonl -kappa-min 0.6 -flip flip.json
./ctx-goldset ingest -judged judge-<run>.md # the filled-in judgements back in as labels
./ctx-goldset stamp                         # refresh digests + corpus contamination stamp
```

`-dry-run` draws and counts the candidates of the four multi-gold generators
without a single model call — the way to check a construction before spending
the one generation run a slice gets.

**A machine verdict is admissible only as far as kappa says it is.** `judge -llm`
sends the pooled candidates through the same on-prem chain the generators use and
appends every verdict to a journal, so an aborted run continues where it stopped
and no cell is judged twice. Beside the filled template it writes a *calibration
sheet*: the mandatory control draws of `pool -control`, each carrying the machine
verdict and an empty `control_judgement` column. A second judge fills that column;
`judge -kappa` then reports Cohen's kappa per slice and overall, and per gate
whether the machine verdicts carry the decision at all. Three things leave a gate
`nicht entschieden` — no calibrated pair, kappa below the threshold, or a marginal
shift between the two judges (exact McNemar, alpha 0.05, the flip case). That
verdict is neither a pass nor a failure: it says the decision may not rest on
these labels yet. `-kappa-min` has **no default** on purpose — a threshold named
after seeing the data describes the result instead of testing it. An endpoint
that is not on-prem aborts the run before the first cell, on both axes the
registry offers (declared locality and the actual host), and model, endpoint and
prompt digest reach the stamp.

**`-draw` / `-calibrate`: calibrating on the population that decides.** The
control draws `judge -kappa` reads are, by construction, disjoint from the pooled
candidates — the run above calibrated the noise probe rather than the judgement
set, and at judge/second-judge positive rates of 0.0200/0.0027 the kappa
denominator is 0.0226, which capped the achievable kappa at 0.2317 whatever the
verdicts were. `judge -draw` replaces the sample: a fully judged CORE of 20
queries (14 local + 6 global, drawn by hash rank so an auditor can reproduce the
selection with sha256sum and sort), a stratified calibration sample over the
remaining queries — S1/S2 split the machine-positive cells by arm overlap, S3/S4
the machine-negative ones by best arm rank — and 60 of the old control cells,
which keep feeding `ControlHitRate` and nothing else. It writes a blind sheet and
a separate answer key: stratum, weight, core membership and machine verdict live
only in the key, because a stratum label *is* the machine verdict. The key
carries no timestamp, so two draws of one seed are byte-identical. `-draw-seed`
has **no default**, for the same reason `-kappa-min` has none.

`judge -calibrate` joins the filled sheet back by `(query_sha256, block_id)` and
reports weighted and unweighted kappa, the judge's sensitivity and precision as
stratified ratio estimates with intervals, the `?`-rate per stratum, and the
control hit rate. The verdict vocabulary gains `?` = "not decidable on this
excerpt", which counts as 0 and is reported as a rubric quality figure. Under the
second-judge-as-gold reading the kappa threshold changes what it decides: not the
gate, but the REACH of the machine labels (`voll` / `nur-kern`). The gate
authority is the metric flip — the same comparison scored against both gold
sources on the core (`armsweep.GoldFlip`) — and an absent flip computation leaves
the gate `nicht entschieden` rather than passing it.

`judge -gold` writes the two gold variants the flip test needs side by side:
`fable-kern` over the core queries and `judge-uebertragen` over the whole slice.
Neither rewrites the slice file — they are new files, so the two readings stay
comparable instead of one replacing the other. `ctx-armsweep goldflip` then
scores one baseline/variant pair twice over one dump, once against each variant,
and writes the paired interval of the per-case difference in the shape
`judge -calibrate -flip` reads. Both variants keep the case indices of the slice
they came from (`WriteJSONLKeepIndex`): a subset renumbered 0..n would carry case
keys that match no dump record, and the flip test would compare an empty
intersection while reporting a clean zero.

**Why the multi-gold slices exist.** G-KI, G-Q and G-REAL carry exactly one gold
id per case. A one-gold slice cannot show the use of an aggregating layer; it can
only punish it as displacement of the single gold block. `G-SESS`, `G-MH` and
`G-GLOB` are multi-gold by construction, and the scorers already handle it —
`evalscore.RecallAtK` and `NDCGRanked` take a gold set.

**`G-GLOB-KONSTR` is a floor check, not a result.** Its gold comes from
`graph_cluster_member`, which is circular against the graph layer it would judge:
a catalog block finding cluster members would score as retrieval quality. It is
therefore reported as its own row (`rollout_criterion: false`) and left out of
`armsweep.ReportSlices()`, which is the set every gate walks. `FloorSlices()`
names it; `CensusSlices()` is what the report census iterates.

**`G-REAL-local` / `G-REAL-global` are regime strata, not slices of their own.**
With `-regime-labels <file>` on `score` or `compare`, the X-W0 label file
(`query_sha256` → `local`/`global`) splits G-REAL into the two regimes the
literature measures *opposite* winners in — 131/19 on the 150 real queries — and
the report carries each half as its own census, metric, effect and (in `compare`)
MDE row. The total row stays where it is and keeps every figure it had; without
the flag the report is byte-identical to the one before the split existed. The
halves are `rollout_criterion: false` for a reason sharper than the floor one: a
stratum is a *subset* of a row that already votes, so letting both vote would
count the same cases twice in G-NOISE's interpretability conjunction and in
G-WIN's regression veto. `StratumSlices()` names them, and every gate keeps
walking `ReportSlices()`. A G-REAL case the label file does not cover aborts the
run (exit 4) instead of falling into a "rest" half nothing in the report names.

Two construction rules are stated in the stamp rather than in a commit message.
The **session window** is half-open `[day 00:00Z, day+1 00:00Z)` over the date in
the daily report's TITLE (a report written after midnight still belongs to the
day it names); span windows are disjoint runs of consecutive reported days, and a
window whose gold set exceeds `-max-gold` is dropped rather than trimmed, because
trimming would label genuinely relevant blocks irrelevant. The **dream-link
floor** is the constant `goldset.MinDreamConfidence = 0.7`: the link audit
measures 56 % correctness overall but 100 % at 0.7 and above, so below the floor
roughly half the gold would be wrong.

`pool` and `ingest` are stage 2 and belong to the pooling construction described
under [Blind relevance judgements](#blind-relevance-judgements-for-g-real-pool--ingest).

Four rules are enforced by the tool, not by discipline:

- **Path guard.** Every write is confined to
  `.project/goldset-retrieval-2026-08/` (private submodule, root-only, untracked,
  CI skips it — gold data is never a `context_blocks` row, or it would sort
  itself into the measurement it exists for). A relative name is joined to that
  root, an escaping or symlinked path is refused, and files are written 0600.
  `--allow-outside-goldset` is the only override and is recorded in the stamp.
- **On-prem generator.** `q`, `sess`, `mh`, `glob` and `glob-konstr` are the
  points where private block content reaches a model. The backend row must
  declare `locality` `local`/`lan` **and** resolve to a private host — the
  registry column is editable state, not a proof, so a mislabelled row still
  aborts (exit 2). The same assertion runs again at stamp-write time
  (`RequireOnPremStamp`): a stamp that would record an external endpoint is
  never written, because the stamp is what a later reader trusts. The row's own
  `extra_body` travels into the request, which is how `enable_thinking=false`
  stays set for qwen38 on SGLang.
- **Redaction sweep.** G-REAL texts and every generated question run through
  `internal/sensitivity` plus a Bearer-token rule. A hit is **discarded**, never
  carried on redacted — a part-redacted query is no longer a real query. The
  draw also filters
  `metadata->>'source' <> 'armsweep'`, so the sweep driver's own logged queries
  cannot be resampled as user queries.
- **Read-only DB.** The connection sets `default_transaction_read_only=on`; the
  tool cannot write to the corpus it measures.

`STAMP.json` carries generator model/endpoint/locality, the frozen prompt's
sha256, the sampling and split seeds, the DERIV/HOLD fingerprint, per-slice `n`
and discard counts (a measured zero is emitted, not omitted), a per-slice
**profile** (construction, gold source, declared bias, rollout role, window rule
or confidence floor, and the model that wrote the questions), the `population`
block that names the ground set a figure was drawn from instead of implying one,
and the corpus `max(created_at)` at draw time — the reference against which later scoring flags
contamination-suspect hits. It also records `build_vcs_revision` from Go's build
stamp — named for the build, not the draw, because in a linked git worktree Go's
repository walk can land on the enclosing checkout. Reports cite a query as
`slice + index + sha256` prefix; full texts stay in the gold directory.

### Arm-weight sweep: ctx-armsweep

`ctx-armsweep` measures the four fusion weights `ctx_rrf` has always carried
(semantic 0.45, `fts_de` 0.20, `fts_en` 0.25, trigram 0.10, k = 60) against the
gold set `ctx-goldset` builds. It rides the admin-gated `arm_ranks` seam on
`POST /api/query`, records the per-arm ranks a real request produced, and
re-fuses them offline under 16 configurations. Four subcommands, deliberately
separate runs (`compare` is described further down):

```bash
cd go && go build ./cmd/ctx-armsweep
./ctx-armsweep prime                                   # pins + embed cache warm-up, nothing scored
./ctx-armsweep dump  -pins pins-<run>.jsonl            # measurement run V0
./ctx-armsweep dump  -pins pins-<run>.jsonl            # measurement run V0' (same pins)
./ctx-armsweep score -dump dumps/<A>.jsonl -dump-b dumps/<B>.jsonl
./ctx-armsweep score -dump dumps/<A>.jsonl -regime-labels x-w0-labels.jsonl
```

Five properties are enforced by the tool, not by discipline:

- **The named slices, or a refusal.** `-slices` defaults to the whole registry
  (`G-KI,G-Q,G-REAL,G-SESS,G-MH,G-GLOB,G-GLOB-KONSTR`, 1 000 cases) and loads
  exactly the names it is given, always in that artefact order. A name the
  registry does not know, an empty list, or a named slice whose file carries no
  cases each abort the run and name the offender. Until wave X-W1a the loader
  walked a three-entry table of its own instead of the names, so the four
  multi-gold slices of M-W5 were dropped in silence: a `prime` over all seven
  names primed 650 of 1 000 cases and exited 0. The stamp follows the loader —
  `prime-<run>.json` names the slices actually loaded, and `gold_sha256` covers
  exactly their files, so a three-slice noise pair and a seven-slice conditional
  dump are refused as incongruent instead of compared.
- **Pins, or nothing.** `prime` captures the translation and temporal-expansion
  results as pins; `dump` refuses a case without one instead of falling back to
  the unpinned path. A partly pinned run is neither a pinned nor an unpinned
  measurement, and the difference would be invisible in the artefact.
- **Drift protocol.** Every dump is bracketed by a corpus census (per type:
  `count`, `max(created_at)`, `max(updated_at)`, null embeddings; plus the
  lifecycle stamps of the labelled blocks). A mutated or vanished gold block, a
  **retrievable** type jumping from 0 to >0 null embeddings, or more than ±0.5 %
  movement in the retrievable block count discards the run — the file is renamed
  to `.aborted`, never deleted, because it is the only record of the drift.
  Excluded types are exempt from the null-embedding rule: the live corpus holds
  thousands there as standing policy.
- **Retry budget.** Two retries per query on transport/5xx faults, then the case
  is EXCLUDED and listed — never replaced by a substitute, which would change
  the population a report is computed over without saying so. A 4xx from the
  seam is a configuration error and stops the run at once. Exclusions apply as
  the UNION over the dump pair.
- **Deterministic reports.** `score` touches no network and no clock beyond one
  header line, so two runs over the same dump produce byte-identical bodies.
  Reports cite a case as `slice + index + sha256` prefix; dumps and pins carry
  the effective query texts and therefore live inside the gold directory at 0600
  under the same path guard, with `-allow-outside-goldset` as the only override
  and `allow_outside_goldset: true` in the report when it was used.

`V0` and `V0'` are the same configuration on two independent dumps: their
disagreement is the instrument's noise floor. Gate **G-NOISE** (Recall@5
discordance ≤ 5 % and a paired 95 % CI of ΔnDCG@10 containing 0) must pass, or
the report marks itself uninterpretable and no variant is a result. Gate
**G-WIN** is decided on `G-Q-HOLD` alone — the half of the seeded 50/50 split
that was not derived on. V1 is the single pre-registered primary comparison at
95 %; the other 13 run at 1−0.05/13 and, if they clear, are labelled "candidate,
unconfirmed".

`score -damping-type <block type>` adds a **second family**: the damping curve of
one block type over ten support points (0.05, 0.10, 0.15, 0.20, 0.30, 0.35, 0.50,
0.70, 0.85, 1.00 — the grid contains every factor the registry currently assigns,
so the status quo is always one of the points). Since migration 142 every dump
row carries its `type_name`, and `type_factor` multiplies into the score *after*
arm membership is decided, so the whole curve is re-fused from one dump without
measuring anything again. It is reported in its own section and deliberately
carries **no** G-WIN verdict: the optimum is a finding for whoever sets the
registry value, and ten extra rows in the variant table would silently loosen its
fixed 13-comparison Bonferroni level. Asking for a curve over a dump measured
before migration 142 is a hard refusal (exit 4), not a fallback — such a dump has
no type names, so the curve would come out flat by construction rather than by
measurement.

`compare` is the fourth subcommand and answers a different question: not "which
weight vector is better" but "what does this CONDITION do" — dump A measured
without a block type, dump B measured with it.

```bash
./ctx-armsweep compare -dump-base dumps/B0.jsonl -dump-cond dumps/B1.jsonl \
    -noise-pair dumps/V0.jsonl,dumps/V0p.jsonl
```

It is a subcommand of its own rather than a second flag on `score`, because
`score -dump-b` is the V0′ **replicate**: there a difference is noise, here it is
the signal, and carrying both roles on one flag would take the report's own
definition of noise away from it. Five properties:

- **No noise floor, no comparison.** `-noise-pair` is mandatory and must name the
  V0/V0′ pair of the SAME campaign; a missing pair or a red G-NOISE refuses with
  exit 3. The refused run still writes its report — the evidence is what an
  operator needs to find the determinism source.
- **Campaign congruence, or exit 4.** All four dumps must share the priming run
  (`pin_run_id`/`pin_sha256`), the gold bytes, `migrations_max`, the post-fusion
  stage state, `instance_kind` (read off the RAW stamps, never off the merged
  report env) and the three ANN knobs of the semantic arm: `hnsw.ef_search` from
  the dump stamp, `hnsw.iterative_scan` and `hnsw.max_scan_tuples` from the
  per-case selector state, which is where migration 142 decides them.
- **Three deltas and two admission rules.** ΔnDCG@10, ΔRecall@5 and ΔMRR@10 per
  slice, each with a paired bootstrap CI, plus McNemar on Hit@5. An effect counts
  as readable only if its CI excludes 0, it exceeds the slice's own MDE, and its
  discordance exceeds that of the replicate pair — a condition that moves fewer
  cases than the instrument moves on its own is not separable from noise.
- **Displacement table.** Per slice: how many top-5 places the shadow types hold,
  which types lost places, and how many displaced blocks were labelled relevant.
  Without labels on a slice the last column stays empty and the report says so.
- **Resolution report.** The half CI width of ΔnDCG@10 between the two identical
  runs IS the smallest difference a slice can show (MDE). Above 2 pp the slice is
  declared "not resolvable for effects of literature size" before any effect is
  read. Dumps are read **streamed** and may be gzip-compressed (`.jsonl.gz`): a
  comparison holds four dumps at once, and at 290 000 records each that is the
  difference between ~40 MB and over 1 GB of resident memory.

Two of those congruence fields have a history worth knowing, because both were
measured as holes rather than reasoned about.

**`instance_kind` is stamped on every dump, not only on shadow dumps.** The
driver reads `server.instance_kind` off the measured instance before the first
measurement request of any non-dry run; an instance that answers the key but
says nothing is stamped `unknown`, never empty. Until wave X-W3a the label was
read only when the run also named `-shadow-types`, so the campaign rule it
serves — F-32, all dumps of one campaign come from ONE instance — guarded none
of the dumps a campaign is actually built from: an empty field compares equal to
an empty field, and a comparison of two measure-copy dumps against a **live**
noise pair ran through with exit 0. Empty now means a dry run or a dump written
before X-W3a. Such a dump still LOADS; a campaign that mixes it with a stamped
one is refused, and the refusal says which side was never stamped.

**A comparison may DECLARE one congruence field as its own condition**
(`-condition-field`). Some measurement waves have as their condition exactly
what the congruence rule forbids — wave X-W2b writes `cluster.inject_max` from 3
to 0, and `post_fusion_stages` is a congruence field, so the contract step of
that wave exited 4. The declaration is deliberately not a generic allow flag: it
names ONE field out of a closed list (today: `post_fusion_stages`), an unknown
name is refused with exit 3 and writes no report, every other field stays as hard
as it was, and the declaration is printed as its own report block with the values
of base, cond and the noise pair. Two consequences ride with it: the replicate
pair must not straddle the declared field (a replicate is a replicate in every
field, or the noise floor measures the condition), and `post_fusion_stages`
switches the metric basis from the offline fusion to the **delivered** ranking.
That switch is the substance of the declaration, not a convenience — the
post-fusion stages run after `ctx_rrf`, so the arm ranks a fusion is recomputed
from cannot carry them (X-W2b measured byte-identical arm signatures across
`inject_max` 0, 3 and 20). Computed on the fusion, such a comparison would return
exactly zero with a full bootstrap CI around it: a tautology in the shape of a
measurement. The delivered window is capped at the server's limit, so the report
names the longest one it saw — `nDCG@10` over a five-entry list is `nDCG@5`.

The census reaches the driver as an **additive, opt-in, server-admin-only**
`drift` section of the existing `POST /api/manage` `stats` action — not as a new
endpoint. A stats request that does not ask for it gets the byte-identical
response it always got, which matters because the statusline polls that action.
`gold_ids` carries at most **10 000** ids per census — enough for the multi-gold
slices, and a hard error above it rather than a silent truncation, because a
dropped id would come back as an absent block and read as drift. The lifecycle
lookup runs in chunks of 1 000 underneath; the answer stays ordered by id, so
the chunking is invisible to the driver.

What the instrument does **not** measure is everything after `ctx_rrf`: gravity,
cluster injection, graph expansion, the aggregate fold and the rerank stage are
recorded (delivered order in the dump, stage config in the env stamp) and never
re-simulated. Details, the operating notes (off-peak, `-concurrency 1`, not
alongside dream) and the `via_post_stage` caveat live in
[`go/cmd/ctx-armsweep/README.md`](../go/cmd/ctx-armsweep/README.md).

### Blind relevance judgements for G-REAL: `pool` / `ingest`

G-REAL is drawn from the access log and therefore has no constructive label —
nobody wrote down which block answers a question a user once asked. Stage 2
supplies the labels from a human judgement over a **pooled, blinded** candidate
list. The tooling is `ctx-goldset pool` and `ctx-goldset ingest`; the judging in
between is a human act and is deliberately not automated.

```bash
./ctx-armsweep prime                                   # writes pool-<run>.jsonl (top-20 per arm; G-REAL + G-GLOB)
./ctx-goldset  pool   -control 5                       # judge-<run>.jsonl + judge-<run>.md + pool-key-<run>.json
#              … a human fills in the first column …
./ctx-goldset  ingest -judged judge-<run>.md           # labels g-real.jsonl, merges the G-REAL profile
./ctx-goldset  stamp                                   # refresh digests, STAMP.json final
```

The construction is pre-registered (design 04 §4.5) and enforced by the tool:

- **Pool.** Per query the union of the top-20 of all four solo arms, taken per
  arm rather than from the fused order, so the pool does not inherit the very
  weighting under measurement. `prime` pools the two slices whose gold is
  JUDGED rather than constructed: G-REAL, and G-GLOB since wave C4-3a. Both
  land in the SAME `pool-<run>.jsonl`, keyed by slice/index/query digest, so a
  consumer selects a slice by reading the entries. `ctx-goldset pool` itself
  still builds its template from `g-real.jsonl` only.
- **Control sample.** Plus five blocks drawn uniformly (seeded) from the
  retrievable set, excluding what is already pooled. Pooling bias is the
  declared residual of this method; the control sample is what makes it a
  NUMBER — the share of uniform draws a judge calls relevant lands in the stamp
  as `control_hit_rate`. Without the key file that rate is not computable, and
  the ingest refuses rather than stamping a zero that would read as "no bias".
- **Blind.** The template carries the query, the block id, the title and a
  600-character excerpt — no rank, no arm, no marker for a control draw, and no
  such field even in the JSONL. The candidate→control mapping lives in a
  separate `pool-key-<run>.json` read only at ingest time. The order is a
  seeded permutation derived from the seed AND the query digest, so every query
  has its own reproducible order instead of one order a judge could learn.
- **No silent negatives.** The verdict vocabulary is `1`/`0`/`y`/`n`; a row left
  at `_` is an error naming the line, never a "not relevant". A query whose pool
  holds nothing relevant KEEPS its place with empty `gold_ids` and is counted as
  `no_relevant` — dropping it would remove exactly the queries retrieval is
  worst at, and every later metric would be computed over a population selected
  by the thing under measurement.

Both template forms are equivalent: the markdown table is one keystroke per row
at a fixed offset for a run of roughly twelve thousand rows, the JSONL is the
same content for a script. `ingest` detects which one it was given, backs up
`g-real.jsonl` to `.bak-<date>` before rewriting it, and merges the G-REAL
profile (`n`, `labelled`, `no_relevant`, `pool_p50`, `pool_max`,
`control_hit_rate`, `pool_run_id`, `pool_seed`, judgement file and digest) into
`STAMP.json` on the RAW document — a field written by another wave survives the
rewrite instead of being dropped by a typed round trip.

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
