# CLI Reference

The `ctx` CLI reads its config from `~/.config/ctx/config` (`CTX_BASE_URL` + `CTX_KEY`). On a TTY most commands render a table; when piped they emit JSON. See [operations](operations.md#configure-the-cli-endpoint) for setup.

## Core

| Command | Description |
|---------|-------------|
| `ctx query question` | Hybrid search + LLM synthesis (formatted, `--json` for raw) |
| `ctx save <cat> <title> - <content>` | Upsert knowledge block |
| `ctx save --tag tag1,tag2 <cat> <title>` | Upsert with tags |
| `ctx save --sensitivity LEVEL <cat> <title>` | Classify on write (`credentials`/`personal`/`internal`/`public`) |
| `ctx search [category] [query:text]` | Compact search (no LLM) |
| `ctx search cluster:<handle>` | Restrict to ONE topic of the cluster map. The handle is the stable `topic` the graph surfaces emit (`GET /api/graph/overview`, ego annotation) — a scope-pure topic, so the answer is exactly that topic's blocks, never a neighbouring scope's half of the same cluster. Server-gated (`cluster.facet_enabled`, default off): while it is off the argument is ignored and you get an ordinary unfiltered search. A malformed handle is a `400` before any database roundtrip; an unknown, foreign or empty topic is an ordinary empty result — never a distinguishable "not found" |
| `ctx get <id>` | Fetch full block |
| `ctx delete <id>` | Soft-delete (archive) |
| `ctx categories` | List all categories |
| `ctx stats` | Database statistics + Dream backlog (`dream_queue`: pickable/cooldown/incoming-forecast) |
| `ctx health` | Healthcheck |
| `ctx version` | Print version |

## Dream & Guard

| Command | Description |
|---------|-------------|
| `ctx guard [list\|stats\|resolve]` | Write Guard management |
| `ctx guard list [--status S] [--category C] [--type T]... [--limit N] [--ids-only]` | List flagged blocks (similarity-sorted, with matched partner). `--ids-only` prints one id per line for piping |
| `ctx guard resolve <id...> <archive\|keep>` | Resolve one or many flagged blocks. The resolution keyword may come first or last, so it composes with xargs: `ctx guard list --status needs_review --type checkpoint --ids-only \| xargs ctx guard resolve keep`. Batch responses report every skipped id with a reason |
| `ctx dream [stats\|review]` | Dream Mode stats — mode, `queue` (backlog + incoming forecast), `backoff` (per-eval-count maturity distribution + effective cooldown); human-readable on a TTY, JSON when piped + link review |
| `ctx dream resolve <source-id> <target-id> <relationship> <confirm\|delete> [--rationale "..."]` | Resolve one dream link from `ctx dream review`: `confirm` pins it (survives the dream replace sweep) and optionally records a durable rationale; `delete` removes it and reverts a supersedes snapshot-marking. Foreign/absent links report `Link not found` |
| `ctx dream enable\|disable\|throttle` | Runtime dream mode control (on/off/throttled) |

## Ingest & maintenance

| Command | Description |
|---------|-------------|
| `ctx brief` | Project briefing from store |
| `ctx persist` | Persist `[PERSIST:cat:title]` markers |
| `ctx ingest <path>` | Ingest Obsidian vault |
| `ctx digest` | Rebuild the topic map, and — when `root_map.enabled` is on — the **root map** (`root-map-<scope>`, one line per topic cluster instead of one per block). Never starts a Louvain rebuild; it renders from the cluster state that exists. With `digest.mode=stub` the old block becomes a ~300 B pointer to the root map, so `ctx search index query:topic-map` still tells you where to look |
| `ctx statusline` | Claude Code status bar |

## Admin: config, secrets, backends

(All require an **admin key** — see [security](security.md#admin-tier).)

| Command | Description |
|---------|-------------|
| `ctx settings [list\|get\|set\|unset]` | Runtime settings overrides (alias `cfg`; reads included). TTY: table, pipe: JSON; `set` takes the value as argument or stdin; API failures (422/409/403) exit 1 with the server's reason |
| `ctx secrets [list\|set\|rotate\|rm]` | Sealed provider credentials (alias `sec`). Write-only: values go in via stdin ONLY (`echo "$KEY" \| ctx secrets set <name>` — an argv value is rejected, it would leak via /proc and shell history); list shows metadata + `referenced_by`, never values; `rm` exits 1 with a 409 while settings reference the secret |
| `ctx backends [list\|create\|update\|delete\|test]` | LLM backend pool. TTY: table with live status, pipe: JSON; `create`/`update` take a JSON spec as argument or stdin; API failures exit 1 |
| `ctx types [list\|get <name>]` | Block-type registry reads (**any valid key**): list the types visible to your key (shipped `_global` set + your tenant's overlay, each with a `source` badge) or inspect one type's policy config. TTY: table, pipe: JSON; a 404 (unknown/foreign type) exits 1 |
| `ctx types set <name>` | Upsert a type (**admin or tenant-admin**): `--display`, `--description`, `--config <json>`, `--default`. Only flags you set are sent. The scope is pinned by your key's role (tenant-admin → your tenant namespace, operator → `_global`), never a flag; targeting a `_global` type as tenant-admin exits 1 (403) |
| `ctx types rm <name>` | Delete a type (**admin or tenant-admin**): a type still referenced by blocks or a builtin exits 1 (409 + count); `_global` types are operator-only |
| `ctx eject [on\|off]` | Eject toggle (canonical, AM-7): drop the GPU-host backends from every chain so the GPU is free (CPU/external stay in as failover). No arg = status (any valid key); `on`/`off` need an admin key. Sends the canonical `eject-mode` manage action; toggles the reserved `eject` disable-profile (092) — persists as that profile row, survives a restart and hits the next chain without one (the legacy `gaming.active` settings key was retired in U01-W5). Legacy alias: `ctx gaming` (kept shape-compatible for existing scripts, N19 — same effect, canonical action) |
| `ctx blocks audit [status\|sample\|start]` | Sensitivity LLM audit: classify `sensitivity_source='default'` blocks over the hard-local `classify` chain. `sample --n 30` = dry-run verdicts without writes, `start [--limit N]` = live run, bare/`status` = progress. See [security](security.md#sensitivity-classification) |
| `ctx blocks classify [status\|dry-run\|start]` | Credentials PATTERN re-audit: the deterministic detector raises every home-scope hit to `credentials` (`sensitivity_source='pattern'`), upgrade-only. `dry-run [--limit N]` = full scan WITHOUT writes (run it first), `start [--limit N]` = live run, bare/`status` = progress |
| `ctx mcp [add\|list\|delete]` | Manage MCP OAuth client registrations. `add <label> [--redirect-uri <uri>]…` (02-W2): repeatable exact redirect allowlist entries — `https` for real hosts, plain `http` only on loopback (RFC 8252); anything else is rejected server-side. Clients registered without it keep `redirect_uris='{}'` and the static S2 allowlist until 03 wires per-client matching (a redirect-less client then matches nothing) |

## Keys & tenants

| Command | Description |
|---------|-------------|
| `ctx keys create <label> --home <scope> [--allowed a,b] [--write a]` | Provision API key (v2.0.0: `--home` required, no default scope; admin key required since migration 052). `--write` (078, E4b) grants extra write scopes; each must be ⊆ home + allowed (else 400) |
| `ctx keys [list\|delete]` | List / revoke provisioned API keys (admin key required since 052); `list` shows `write=[…]` when a key carries write scopes |
| `ctx tenant [list\|get\|create\|update\|delete]` | Tenant lifecycle (**server-admin key**). `create <slug> <name>` is the compound bootstrap: tenant + initial scope `<slug>:main` + owner key (plaintext shown once). TTY: table, pipe: JSON; API failures (403/404/400) exit 1 |
| `ctx tenant grant [create\|list\|delete]` | Cross-tenant read grants (**server-admin key**): grant another tenant read access to one scope; delete revokes immediately |
| `ctx tenant limit set <id> --max-scopes N --max-keys N` | Structural caps (**server-admin key**). Replace semantics — both flags required; negative = unlimited, 0 = frozen |
| `ctx tenant usage [id]` | Scope/key counts vs limits. No arg = own tenant (tenant-admin suffices); targeting another tenant needs a **server-admin key** |
| `ctx quota [set]` | Per-tenant cost/call budget (read: tenant-admin; `set`: **server-admin key**) |
| `ctx block-grant [create\|list\|revoke]` | Row-level read sharing of a single block with another tenant (**server-admin key**) |

Tenant semantics are documented in [multi-tenancy](multi-tenancy.md).

## Projects

A project binds one repo corpus to exactly one tenant scope (Model C): the scope is the isolation boundary, so a scope belongs to one tenant and each project's issues live only in its own scope.

| Command | Description |
|---------|-------------|
| `ctx project` | = `show`: detect this repo's identity, then look up its registered project. TTY: detail; pipe: JSON |
| `ctx project detect` | Resolve the identity locally, no server call. Precedence: a GitHub `origin` remote → `github:owner/repo`, else a single-root git repo → `git-root:<sha>` (survives clones), else a manual slug (prompted on a TTY, an **error when piped**). TTY: human lines; pipe: JSON `{identity, source}` |
| `ctx project init [--identity ID \| --repo URL]` | **Provision** this repo: the server-admin `project-provision` compound mints a whole isolated tenant-per-repo in ONE transaction — tenant + scope `<slug>:main` (slug derived from the identity; on overlength truncated + a hash suffix so two long identities never collide) + owner key + a **repo-agent key** + an LLM-quota seed. Idempotent — re-running with the same identity is a No-op that returns the existing project (the reveal-once keys are shown ONCE, at first provision). The repo-agent key is minted STRICTLY to the agent-key template (`home_scope=<scope>`, `allowed_scopes=[]`, `write_scopes=[]` — it can write ONLY its own scope, never `shared`) and stored **0600 under `~/.config/ctx/projects/`, NEVER in the working directory** (a secret in the repo would be a commit hazard). Writes `.ctx-project` (the identity + scope/key-path pointers as comments, never a secret). Needs a **server-admin key** (tenant creation is server-admin, like `ctx tenant create`). The legacy register-into-an-existing-tenant path lives on `ctx api POST /api/project` |
| `ctx project show` | Detect, then show the registered project (or a "not registered" hint) |
| `ctx project list` | Every project your key can read (scope intersection). TTY: table, pipe: JSON |

**`.ctx-project` is a detect shortcut, not a trust anchor.** For `github:`/`git-root:` identities `detect` honors the file **only when it matches** the independent git detection; a mismatch (e.g. a cloned foreign repo with a planted file that would redirect issue writes into someone else's corpus) forces interactive confirmation on a TTY and **errors when piped** (exit ≠ 0). Only `manual:` identities — which have no independent source — are authoritative from the file.

### Issues

Work with a project's issues. The project is detected from the current repo (like `ctx project`), or named explicitly with `--project` (a project id **or** an identity). The state-change verb is **`status`** (not `state`/`move`). On a TTY the output is a table / human lines; piped, it is the raw server JSON (stable, scriptable). A `success:false` envelope exits 1 with the server's reason.

| Command | Description |
|---------|-------------|
| `ctx project issues [list]` | List issues, newest first. Filters: `--state <status>`, `--label <l>` (repeatable), `--q <text>`. Keyset-paginated for 10k+ issues: the response carries an opaque cursor; pass it back with `--after <cursor>` for the next page. `--sort created` gives a gap-free immutable traversal for export |
| `ctx project issues show <block-id>` | Show one issue in full plus the first page of its comment thread (a foreign/unknown id reads as not found) |
| `ctx project issues create [title]` | Create an issue. Title is the positional arg **or** `--title`; body is `--body` **or** stdin (`echo body \| ctx project issues create --title t`). `--label` (repeatable), `--status` (a valid entry state for the type policy) |
| `ctx project issues comment <block-id>` | Add a comment to an issue's thread. Body is `--body` or stdin; `--author` labels it (default: anon) |
| `ctx project issues status <block-id> <new-status>` | Move an issue to a new workflow status. The transition is validated against the type's policy data server-side; an out-of-policy target exits 1 (422) with the reason |
| `ctx project issues sync [--status]` | Trigger a forge (GitHub) pull sync for the project (needs **write** access to its scope — decision E6=a), or with `--status` poll the current/last run without starting one. A double-start of the same project, the per-project rate limit, or the daemon-wide concurrency cap exit 1 (409/429) with the server's reason |

Issue/comment titles, bodies and labels are attacker-controlled text (any user can open an issue in a mirrored repo). Human/TTY output runs every such value through a terminal-escape **allowlist** — all C0 controls except newline/tab, DEL, and all C1 controls (raw byte or code point) are stripped — so a crafted title cannot drive escape sequences into your terminal, an agent log or CI output. The piped JSON is the machine contract and is left verbatim.

### Board

The kanban board view: the project's issues grouped into the workflow-status columns of its type config (never hardcoded). One data path — the CLI eats the same `/api/project/{id}/board` wire the web UI does.

| Command | Description |
|---------|-------------|
| `ctx kanban [--project ID] [--limit N]` | On a **TTY**, an interactive, read-only board (bubbletea/lipgloss): arrows or `hjkl` navigate columns/cards, `q` quits (v1 does not mutate — re-status via `ctx project issues status`). **Piped**, the raw board JSON `{columns:[{status,count,issues,cursor}]}` (stable, scriptable). Each column carries a total `count` and only a **first page** of cards, so the board scales to 10k+ issues per column; `--limit` sets the page size (server default when 0). Page a hot column via its `cursor` with `ctx project issues list --state <col> --after <cursor>`. Titles are sanitized through the same escape allowlist on a TTY; the piped JSON stays verbatim. A `success:false` envelope (e.g. a foreign project) exits 1 |

## Raw REST access

| Command | Description |
|---------|-------------|
| `ctx api <method> <path> [json]` | Generic authenticated passthrough to any `/api` route (method is GET/POST/PUT/PATCH/DELETE; JSON body via arg or stdin, sent verbatim). Prints the response JSON; a `success:false` envelope exits 1 with the server's reason. This is the script-level reach for the REST surfaces (project/types/issues) that are not `manage` actions — e.g. `ctx api GET /api/project`, `echo '{"display_name":"X"}' \| ctx api PATCH /api/project/<id>` |
