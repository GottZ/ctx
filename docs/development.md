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

### Areas

- **Settings** — renders the full [Settings API](api.md#settings-api) catalog generically from registry metadata: one category card per key prefix, widgets dispatched by registry type (an unknown future type degrades to read-only), source badge (`default`/`env`/`db`) and env-var name per field. Hot and `coupled:embed-cache` keys edit live (one `PUT` per changed key, a `422` lands inline); restart/coupled keys render read-only. Fields with a `db` override get a reset affordance. The three cross-field rules (thresholds, dual-runner `num_ctx`, `blend_weight`×graph) are mirrored client-side as inline previews while the server-side candidate build stays authoritative. Includes the **Backends** sub-route (`/settings/backends`) — backend-pool editor with a trust dropdown + elevation-confirm dialog, roles multi-select, model_map line editor, priority up/down and per-row reachability test — plus the write-only **secrets vault** with reference tracking.
- **Graph** (`/graph?focus=<uuid>`, deep-linkable) — renders dream-link ego networks via sigma (WebGL) over one graphology instance (deliberately outside Svelte reactivity — the runes proxy overhead on thousands of node objects is the documented reason). Entry is the FTS search; a hit/node click focuses that block's ego net (`GET /api/graph/ego`, 2 hops). Double-clicking a node expands it (+1 hop merge); the layout is ForceAtlas2 in a web worker (Blob-URL — CSP carries `worker-src blob:`), running 3–10s scaled by graph size after every merge. Client memory is hard-capped: over 5000 nodes / 20000 edges the nodes farthest from focus (BFS distance, LRU tie-break) are evicted down to 4000 — pinned nodes and focus survive. One filter state (link class, min confidence, category, created window) drives both sides: loaded elements filter instantly through the sigma reducers (zero server roundtrips) while new fetches mirror the filters as ego-query params. Single-click opens a detail sidebar (content lazy through scope-checked `manage get`; content renders as a text node, never `{@html}`).
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

### Wire-contract freeze (workflow UI)

The workflow-UI API client (`go/web/src/lib/api/issues.ts`, `types-registry.ts`) and the SPA e2e/vitest fixtures both eat the **same** contract-freeze JSONs in `go/web/src/lib/api/__fixtures__/*.json` (issue list/detail/comments/board/mutate, project list, sync status, type list). Those files are re-serialized from the live handler structs (W6/W7/W11/W4/types) by the Go golden `TestContractFreezeGolden` (`internal/handler/contract_freeze_golden_test.go`) — a drift on either side turns it red before deploy (closes the fixture-drift gap: the FE mocks can no longer diverge silently from the Go wire). To regenerate the JSONs after an intentional wire change, review the diff from:

```bash
cd go && UPDATE_FREEZE=1 go test ./internal/handler -run TestContractFreezeGolden
```

The path prefix of the whole workflow surface lives in exactly one constant each (`ISSUES_BASE` = `/api/project`, `TYPES_BASE` = `/api/types`); the client functions and the e2e fixture namespace matcher (`go/web/e2e/issue-fixtures.ts`) both import it, so an un-mocked path inside the namespace hard-fails loudly (599) instead of a benign `{success:true}`.

### Live tier (PV10) — real ctxd, production write paths

Beside the browser-mocked suite there is a second, small tier (`go/web/e2e/live/`, ≤ 15 `@live` specs) that runs against a **throwaway** ctxd + Postgres brought up per run by `docker-compose.e2e.yml` (`bun run e2e:live` → `run-live.sh`). It proves the classes the mock tier cannot: real server **enforcement** (tenant isolation with a positive control), fixture-**shape** truth (W10), and real **SSE transport**. The corpus is seeded **only through production write paths** (`tenant-create` → `store` → `issue-create`), so the seed itself is an integration test of those handlers and no hand-written shape can drift from the server. A three-layer **fail-closed target gate** (`seed.ts`, design 06 §3.6) refuses to write to anything but the job-local instance (env-gate + host binding, a per-run bootstrap-key handshake via `GET /api/whoami` — the PV10a `CTX_BOOTSTRAP_ADMIN_KEY` mints the first server-admin key only on an empty DB, [operations](operations.md#environment-variables), and the key is valid solely against the instance that dies with the stack). The tier makes **no pixel assertions** — no screenshot baselines here. Runbook, the three negative gates (W10 shape-mutation / target-gate refusal / leak-detector), the trace-secret handling and the **release-gate rule** (a version tag is pushed only after a green nightly `web-live` run — the "CI is truth" rule extended to the live tier, with the staging-redaction caveat) live in [`go/web/e2e/live/README.md`](../go/web/e2e/live/README.md). CI runs it nightly (`schedule`) + on `workflow_dispatch` + on PRs labelled `e2e-live` — never on every PR (the mock `web` job stays the per-PR frontend gate).

## Visual baseline governance (Web e2e)

Screenshot baselines (`go/web/e2e/__screenshots__/`) are the frozen "objectively good" reference for the UI: the taste judgement is made once, at baseline approval — afterwards every pixel deviation is a measurable diff, not an opinion. Baselines are only valid when rendered inside the digest-pinned toolchain container (`go/web/e2e/toolchain.lock` pins the image digests; `bash go/web/e2e-visual.sh --update` is the only regeneration path — CI has no update path and only compares).

The comparison runs at `maxDiffPixels: 0` (with a calibrated per-pixel `threshold` for compositing anti-alias jitter) — a deliberately strict global policy. The **only** sanctioned relaxations are per-shot (design 05-§8.E3 / 06 §4.3 escalation ladder: mask → stylePath → per-shot `maxDiffPixels`), never a global loosening. A per-shot budget lives on a `PageState.visualTolerance` (contract registry) with a mandatory reason + issue anchor and is validated structurally. The single current use is the board **empty** state: the self-hosted Mono draws accent-blue glyph edges against the wide `surface-0` field, and under parallel-worker load the pinned SwiftShader rasteriser wobbles one such edge pixel by ±1 LSB (deterministic in isolation, not retry-clearable on that sparse shot). Its `maxDiffPixels: 4` tolerates the measured 1-pixel jitter and stays orders of magnitude below any real regression on that shot.

Any commit that touches `__screenshots__/` or adds a11y-debt entries (`go/web/e2e/a11y-baseline.json`) must carry a **`[baseline]`** marker in its message plus a one-line reason. The `commit-msg` hook rejects it locally (fast feedback), and the CI marker gate rejects it on the PR (enforcement that survives `--no-verify` and dead hooks). Shrinking the a11y debt (ratchet) needs no marker. Baseline changes live in their own commits, never mixed with feature code, and are **batched per wave group** (one consolidated `[baseline]` commit per group, squashed before merge) to keep the non-delta-compressible PNG history rate bounded.

**Playwright/toolchain upgrade runbook** — an image bump and the baseline regeneration are ONE coupled step, never separate:

1. Bump the image digests in `go/web/e2e/toolchain.lock` (the tag next to it is a human-readable label, the digest wins).
2. Regenerate: `bash go/web/e2e-visual.sh --update` (builds the container from the new pins, refreshes changed baselines inside it).
3. Commit lock bump + regenerated baselines together as ONE `[baseline]` commit stating the upgrade as the reason.

## Git hooks

Enable with `git config core.hooksPath .hooks`:

- **pre-commit** — golangci-lint on staged Go files.
- **commit-msg** — enforces a documentation review for `feat:` commits and schema migrations (`go/migrations/`): the commit must stage `README.md` **or** a `docs/*.md` file. Also enforces the `[baseline]` marker (above).
- **pre-push** — requires annotated `v*` tags for version releases.
