[![Release](https://img.shields.io/github/v/release/GottZ/ctx)](https://github.com/GottZ/ctx/releases)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8)](https://go.dev)
[![License](https://img.shields.io/badge/license-MPL--2.0-blue)](LICENSE)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/15838/badge)](https://www.bestpractices.dev/projects/15838)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-336791)](https://www.postgresql.org)

# ctx

**The memory your LLM pretends to have.**

ctx is a knowledge store for LLM agents that you run yourself. Your agents save what they learn as small blocks: a
decision with its reason, a fix with the error it fixed. Weeks later a fresh session asks "why did we pin
pgvector?" and gets a short answer written from those blocks, with their ids. ctx finds a block by its meaning, by
its exact words and by time. Claude Code, claude.ai, Hermes and any other MCP client can share one memory, and your
scripts reach it through a CLI and plain HTTP. When your agents save and look up is up to the instructions you give
them: ctx is the store, not the habit.

```
ctx query "why did we pin pgvector?"                    ask: a short answer from your blocks, with sources
ctx save decisions "pgvector pin" - "0.8.7, because …"   remember: a new block, or replace the one with that title
ctx search decisions query:pgvector                     look around: matching blocks, no model call
ctx get <block-id>                                      read one block in full
```

## What you get

- **Answers, not a list of hits.** `query` searches four ways at once: by meaning, by English and German full
  text, and by similar titles. It merges the rankings and lets your chat model write a short answer from the
  blocks it found, with their ids. `search` gives you the blocks without the model call.
- **Time counts.** Every block keeps the day it was written and the full dates it names (2026-03-29, 29.03.2026,
  March 2026). A question that names a time, such as "every Tuesday", "at Christmas" or "immer dienstags", ranks
  the matching blocks higher.
- **Duplicates caught, links on request.** Guard compares each new block with the closest ones and, depending on
  the block type, archives a duplicate or flags it for you. Dream Mode, once you switch it on, reads blocks in the
  background, links related ones and marks what a newer block replaces.
- **One store for every agent.** 16 MCP tools for Claude Code, claude.ai or any MCP client, a CLI for scripts,
  cron and hooks, and a plain HTTP API. In Claude Code, two hooks give each subagent the project's briefing when it
  starts and save the findings it marks when it ends.
- **Several teams on one server.** Tenants hold scopes, scopes hold blocks. A key reads only the scopes it was
  given, and another tenant's data only through a grant you make, for one scope or one block. Quotas and key
  limits per tenant.
- **Your models, wherever they run.** Ollama, any OpenAI-compatible server or hosted API; chat also on the
  Anthropic Messages API. Each role (answers, embeddings, reranking, Dream Mode and a few more) has its own list of
  backends in priority order, so a CPU backend can stand in for a GPU one.
- **Your models for other programs, if you want.** An OpenAI-compatible API under `/v1` (chat, embeddings,
  rerank) lets other programs use those models, with keys, limits and quotas of their own. ctx's own questions are
  served first. Off until you switch it on.
- **A web UI inside the server.** Settings, model backends, the link graph, a block browser, live status, a chat
  that searches the store for you, project issues with a board. You sign in with an API key, or with a sign-in
  provider you set up.

## How it works

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/how-ctx-works-dark.svg">
    <img alt="How ctx works: four ways in (MCP, CLI, HTTP, web UI) lead to one Go server on PostgreSQL. Storing a block records its dates, computes an embedding and checks for duplicates. A question is searched four ways, weighted by time and by links, and answered by the chat model. A topic map groups the blocks, and the optional Dream Mode links them in the background." src="docs/how-ctx-works.svg" width="100%">
  </picture>
</p>

One Go server (`ctxd`) on PostgreSQL 18 with pgvector and TimescaleDB. The models run wherever its backend pool
points. Dream Mode, shown in the picture, runs only when you switch it on. A block takes this path:

| Step | What happens |
|---|---|
| 1. Store | A block arrives over MCP, the CLI or HTTP and is saved at once. A block with the same category and title in the same scope is replaced. ctx records the full dates the text names and the time it was written. |
| 2. Embed and check | In the background the embedding model turns the block into a vector. Guard compares it with the closest blocks and archives or flags a duplicate. |
| 3. Ask | ctx reads the question for time words, embeds it, and searches four ways: by meaning (pgvector), English full text, German full text and title similarity. It fuses the four rankings into one. |
| 4. Weigh | Blocks near the times the question names rank higher. Once Dream Mode has written links, blocks linked to the best hits join in, and blocks that a newer one replaced drop out. A reranker and a boost for the strongest topics can be switched on. |
| 5. Answer | The chat model is told to answer from the retrieved blocks only. It cites them, and ctx lists their ids. |
| 6. Consolidate | With Dream Mode on, a background loop picks one block at a time, finds candidates and asks the chat model how they relate. It writes at most five links per round, including "this block replaces that one". |

`search` takes a lighter path: full-text matching only, without embedding and without a model call.

There are four ways in:

| Way | Use it for |
|---|---|
| MCP (`/mcp`) | agents in Claude Code, claude.ai, Hermes or any MCP client; API key, or OAuth 2.1 for remote clients |
| CLI (`ctx`) | the shell, scripts, cron, Claude Code hooks and the statusline; administration |
| HTTP (`/api/…`) | your own programs |
| Web UI (`/`) | settings, backends, graph, blocks, status, chat, issues |

## Before you start

- **A Linux host with Docker Compose** for the server. ctx runs as a Linux container image; that is the only
  supported runtime. The compose file builds it, and a database image with PostgreSQL 18, pgvector 0.8.7 and
  TimescaleDB 2.31.0.
- **A chat model and an embedding model** that ctx can reach over HTTP: Ollama, an OpenAI-compatible server or a
  hosted API. Chat also works through the Anthropic Messages API, which serves no embeddings. The embedding model
  must return at least 1024 dimensions. ctx keeps the first 1024, so pick a model whose first 1024 dimensions work
  on their own, such as the reference Qwen3-Embedding-8B. The tested reference for chat is qwen3.6:27b, with an
  optional bge-reranker-v2-m3 reranker. None of them is required.
- **For the CLI:** Linux or macOS on amd64 or arm64, or Windows on amd64. To check the download, the GitHub CLI
  (`gh`), signed in with `gh auth login`, or the provenance file of the release (below). Building from source
  needs Go 1.27.

What does not work, or only partly:

- **ctx does not make an agent remember by itself.** It brings no rules for when to look up or save. An agent
  that has ctx only as one more tool behaves as if it had no memory; see
  [Bring your own workflow](#bring-your-own-workflow).
- **Nothing answers before the models do, and CPU alone is slow.** Until the backend pool has a chat and an
  embedding backend, `/health` answers `unhealthy` (HTTP 503) and questions fail at the embedding step. On CPU
  alone, the 27B reference model needs about 4.5 to 5.5 minutes per answer; on a GPU, up to a minute.
- **Full text in German and English only.** The full-text search knows the word forms of these two languages;
  content in other languages is found by meaning and by its title. A question with umlauts or common German words
  is translated to English before the search, and no setting turns that off.
- **New text is found by meaning a little later.** Embeddings are computed in the background. A block you just
  saved or changed is found by its words and its title at once, and by meaning once its new vector exists.
- **No earlier versions.** Saving under the same category and title replaces the whole block, tags and metadata
  included, and ctx keeps no revision of the old one. Deleting a block archives it; `ctx get --include-archived`
  still reads it by its full id.
- **Changing the embedding model is a migration.** Every stored vector has to be computed again in a supervised
  re-embedding run (`ctx admin embed-migration`); it is not a setting you flip.

## Install

**The CLI** (Linux and macOS). The download goes to a temporary folder, `gh attestation verify` checks that this
repository's release workflow built the file, and only then is it installed. Writing to `/usr/local/bin` needs
root; any other folder on your `PATH` works too.

```bash
f=ctx-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
tmp=$(mktemp -d)
curl -fsSL "https://github.com/GottZ/ctx/releases/latest/download/$f" -o "$tmp/$f" \
  && gh attestation verify "$tmp/$f" -R GottZ/ctx \
  && install -m 0755 "$tmp/$f" /usr/local/bin/ctx
```

On Windows, download `ctx-windows-amd64.exe` from the [latest release](https://github.com/GottZ/ctx/releases/latest)
and check it the same way. Each release also carries `SHA256SUMS` and `ctx.intoto.jsonl`, the signed provenance as
a file: `gh attestation verify <file> --bundle ctx.intoto.jsonl -R GottZ/ctx` checks against it without signing in.

To build the CLI yourself instead, clone the repository and build in its `go` folder without leaving the root
folder, then install the binary the same way:

```bash
git clone https://github.com/GottZ/ctx && cd ctx
go -C go build -o ctx ./cmd/ctx && install -m 0755 go/ctx /usr/local/bin/ctx
```

**The server.** Everything below runs in the root folder of a clone (`git clone https://github.com/GottZ/ctx &&
cd ctx`, or the clone from the build above). First the `.env` file. It will hold secrets, so it is created
readable by you only, and a `.env` you already have is kept and gets the same rights. The last line adds your
first key, which ctx makes at its first start:

```bash
[ -e .env ] || (umask 077 && cp .env.example .env)
chmod 600 .env
grep -q '^CTX_BOOTSTRAP_ADMIN_KEY=' .env || echo "CTX_BOOTSTRAP_ADMIN_KEY=$(openssl rand -hex 32)" >> .env
```

Then set the passwords and `CTX_SECRETS_KEY` in `.env`, each a value from `openssl rand -hex 32`, and start
ctx. Compose builds ctxd and the database image and starts both:

```bash
[ -e docker-compose.override.yml ] || cp docker-compose.override.yml.example docker-compose.override.yml
docker compose up -d ctx
```

The `ctx` service publishes no port of its own; it carries labels for a reverse proxy. The first line above is
optional: it gives ctx the port `127.0.0.1:8080` and never replaces a `docker-compose.override.yml` you already
have. In that case, add the `ports:` entry of the example under `services: ctx:` in your file yourself. With a
proxy of your own in front, skip the line. Without a proxy, compose's warnings that `CTX_HOSTNAME` and
`CTX_SSL_DOMAIN` are not set do no harm.

Copy `CTX_SECRETS_KEY` from `.env` into your password manager. It seals the provider keys in the database, and
backups do not contain it: without it, those keys are gone. Every setting and environment variable is in
[Operations](docs/operations.md#environment-variables).

**Upgrading from v4? Reach v4.38.0 first.** v5.0.0 no longer reads the backend connection variables such as
`CTX_CHAT_HOST` or `CTX_EMBED_MODEL`; backends live in the pool (`ctx backends`) and nowhere else. One boot on
v4.38.0 carries such a `.env` configuration into the pool and names every variable and settings row that is left.
From v4.19.0 on, skipping that release is not fatal, but you enter your backends again with `ctx backends seed`.
Below v4.19.0, v5 refuses the database. Path and rollback:
[Operations](docs/operations.md#migration-133-the-backend-tuple-rows-are-deleted--and-the-upgrade-hop-that-has-to-come-first).

## Set up once

1. **Point the CLI at the server and give it models.** Run `ctx init` with the server's address (with the
   override above: `http://127.0.0.1:8080`) and the key from `CTX_BOOTSTRAP_ADMIN_KEY`. If the key is mistyped or
   the server rejects it, init says so and asks again; it never saves such a key. Then it asks for your model host,
   its protocol, a chat model and an embedding model, seeds the backend pool and tests what it wrote. Give the
   host as the ctx container reaches it: `localhost` there is the container itself. `ctx init` also offers two
   Claude Code hooks for subagents and the statusline; `ctx init --remove` takes them out again.

   ```bash
   ctx init
   ctx health
   ```

   `ctx health` shows the database and the model backends. For scripts, the backend step alone runs without
   questions: `ctx backends seed --host http://gpu-host:11434 --model qwen3 --embed-model qwen3-embedding`.

2. **Make keys.** The bootstrap key is a server-admin key. Keep it in your password manager, remove its line from
   `.env`, and give the CLI an everyday key in its config file afterwards. The first line below makes an everyday
   key that writes to the scope `private`; the second makes a tenant with its own scope and an owner key, shown
   once:

   ```bash
   ctx keys create laptop --home private
   ctx tenant create friend "Friend Corp"
   ```

3. **Connect your agents.** Claude Code and other MCP clients that send a header:

   ```json
   { "mcpServers": { "ctx": { "type": "http", "url": "https://your-ctx-host.example/mcp",
                              "headers": { "X-Context-Key": "your-api-key-here" } } } }
   ```

   claude.ai and other remote clients sign in with OAuth 2.1: register one with `ctx mcp add "Claude AI"` and add
   `https://your-ctx-host.example/mcp` as a connector. For Hermes, see [Hermes plugins](#hermes-plugins).

4. **Optional.** Dream Mode: set `CTX_DREAM_ENABLED=true` in `.env` and run `docker compose up -d ctx` again. It
   keeps calling your chat model in the background; `ctx dream throttle` adds a pause between its calls. The
   OpenAI-compatible API: [enabling it](docs/operations.md#upgrading-to-5330-enabling-the-openai-compatible-api).

The full walk-through, with every option, is in [Operations](docs/operations.md#first-run-ctx-init).

## Bring your own workflow

ctx is a skeleton. After the install it stores, finds and links what your agents give it, over MCP, the CLI and
HTTP. It does not decide when your agent looks something up or writes something down, and it does not make a
session start or end well on its own. Today ctx is deliberately neutral: it brings no working style of its own and
no ready-made skills, because every team works differently. Below is a way of working we suggest. Adapt it to your
team and write it into your own skill, rule file, hook or prompt. Suggestions for ready-made skills will come
later.

**Session start.** Before its first answer, your agent:

1. loads the conventions it works under into its context;
2. lists what the last session left open, with the blocks those items point to: `ctx search tags:open`, or the
   MCP tool `search` with `tags: ["open"]` (`open` is a tag you agree on, see step 5 of the close);
3. asks `query` whole questions about the task ("why did we pin pgvector?", not "pgvector pin"), and reads a
   block in full with `get` only when it holds its id;
4. says in two or three sentences what it loaded, then starts.

**During the work.** Your agent looks up before it decides, and checks live state before it acts on a block: a
block says where to look, not what is true now. It saves each finding when it happens: one concept per block, a
precise title, a category, tags, the source and the date. It searches first and changes an existing block instead
of adding a second one (`ctx manage update <block-id> '<json>'` or the MCP tool `update`); `ctx save` with the
same category and title replaces the whole block.

**Session close.** A session is closed when every "done" in its report rests on the output of a command, not on
what the session remembers. Each step has a check, and what was not checked is reported as not checked. If a step
is red, your agent reports the red state instead of closing.

| Step | Check |
|---|---|
| 0. Who asks | A person, or an automation such as a timer, a loop or a hook? An automation continues the task; it does not end it. |
| 1. Working tree | `git status --porcelain` is empty, or every change left is a decision you can name. Nothing is unpushed without a reason (`git log @{upstream}..HEAD`). |
| 2. Checks | The project's own tests and checks ran in this session. Green means "nothing known is broken", not "everything works". |
| 3. Deployment | If something was deployed: the commit in the repository, the deployed commit, the running service and its health check agree. The deploy tool's success message is not that state. |
| 4. Store | Every finding a later session needs is in ctx, one concept per block, an existing block updated rather than copied (`ctx search`, then `ctx manage update`). Each fact lives in one place: git holds the diff, ctx the essence. The test: would a later session find it without this chat? |
| 5. Open items | Each one neutral, with its source (file and line, or a quote with its date) and what it unblocks, without an urgency nobody gave it; stored with the tag `open`, and archived with `ctx delete <block-id>` once it is done. |
| 6. Report | Per step, what was checked and with what result; what was not checked or stays open; the ids of the blocks the next session starts from. |

This is not a handover note that retells the session. The findings are already in the store, per topic; the close
checks that, and leaves only the open items and the pointers to them.

What there is to start from:

- [ctx as agent memory](docs/ctx-as-agent-memory.md): the loop for a session and for a subagent, and what goes
  wrong when an agent treats the store as a tool, including why a retelling handover note is one of those ways.
- [Using ctx effectively](docs/using-ctx-effectively.md): rules to put into your agent's instructions, and how to
  keep stored memory from drifting.
- `ctx init` installs, if you agree, two Claude Code hooks for **subagents**. `ctx brief --hook` hands each new
  subagent the blocks of the category `agent-briefing` that are tagged with the project. `ctx persist --hook`
  stores each finding the subagent marks with a `[PERSIST:category:title]` line in its last message
  ([Claude Code integration](docs/operations.md#claude-code-integration-optional)). Nothing covers the start or
  the end of your own session.
- The Hermes plugin [`ctx_checkpoint`](integrations/hermes/README.md) shows what a finished integration looks
  like: it archives the transcript in ctx before Hermes compacts a conversation, and with `memory: true` it adds
  recall before each turn and the skill `ctx-memory`, which tells Hermes how to query and store during the work.

## Everyday use

| You want to | Command |
|---|---|
| Ask a question of your stored knowledge | `ctx query "question"` |
| Save a finding | `ctx save <category> <title> - <content>` |
| Replace a block | `ctx save` with the same category and title |
| Change one field of a block | `ctx manage update <block-id> '{"tags":["a","b"]}'`, or the MCP tool `update` |
| Browse without a model call | `ctx search [category] [query:text] [tags:a,b]` |
| Read one block | `ctx get <block-id>` |
| Archive a block | `ctx delete <block-id>` |
| See the store and its health | `ctx stats`, `ctx health` |
| Review flagged duplicates | `ctx guard list`, `ctx guard resolve <id> keep\|archive` |
| Import an Obsidian vault | `ctx ingest <path> --dry-run`, then without `--dry-run` |
| Call any HTTP route with your key | `ctx api GET /api/whoami` |

Agents do the same over MCP: `query`, `store`, `search`, `get`, `recent` and eleven more tools for updates,
archiving, confirmed writes, issues, duplicate review and binary blobs ([MCP reference](docs/mcp.md)).

## Hermes plugins

Three plugins for [Hermes](https://github.com/NousResearch/hermes-agent) live in this repository:

| Plugin | What it does | Install |
|---|---|---|
| [`ctx_checkpoint`](integrations/hermes/README.md) | The ctx memory provider. Before Hermes shortens (compacts) a long conversation, it archives the direct transcript in ctx; on request, no compaction runs without that copy. With `memory: true` also recall before each turn, `ctx_*` agent tools, a mirror of Hermes' memory files, fact extraction, session digests and the skill `ctx-memory`. Talks to ctx through Hermes' own MCP connection. Its memory engine was contributed by derLonius (W4-NERF) as [hermes-ctx-plugin](https://github.com/W4-NERF/hermes-ctx-plugin). | `hermes plugins install ctx_checkpoint`, then `hermes memory setup ctx_checkpoint` or the memory form in Hermes Desktop |
| [`ctx_handover`](integrations/hermes/plugin/ctx_handover/README.md) | Moves a running Hermes session, with its conversation and project folder, from one of your machines to another and back. Works without ctx. | `hermes plugins install ctx_handover` and `hermes plugins enable ctx_handover`, on both machines |
| [`ctx_discord_policy`](integrations/hermes/plugin/ctx_discord_policy/README.md) | Lets a Hermes bot talk on your Discord servers while its tools stay with the operators in your Hermes configuration; each server is set up inside Discord with `/policy`. Works without ctx. | `hermes plugins install GottZ/ctx/integrations/hermes/plugin/ctx_discord_policy --enable`, then restart the gateway |

`ctx_checkpoint` ([catalog page](https://hermes-agent.nousresearch.com/docs/plugins/ctx_checkpoint)) and
`ctx_handover` are listed in the Hermes plugin catalog; `ctx_discord_policy` is installed from this repository.

## Safety in short

- **A key reads only what it was given.** Every key writes to its own scope and reads the scopes it was allowed;
  keys of the default tenant get its `shared` scope by default. Another tenant's data needs a grant. A key whose
  tenant cannot be resolved is refused.
- **Admin work needs an admin key.** Keys, MCP clients, settings, secrets and the backend pool need a
  server-admin key, or a tenant-admin key for its own tenant.
- **Provider keys are sealed.** API keys of model providers are stored encrypted (AES-256-GCM) under
  `CTX_SECRETS_KEY`, written through a write-only API that never returns them.
- **Sensitive blocks stay on trusted models.** Every block has a sensitivity, and an unclassified block counts as
  `credentials`. Every backend has a trust level, and a block reaches only backends trusted for it. A scanner
  raises blocks that contain credentials.
- **Agent writes can wait for a yes.** A key with `confirm_writes` gets its MCP writes staged; they run only when
  `confirm` is called with the exact hash.
- **Text from outside is marked.** Blocks that hold tool output or transcripts carry an `untrusted` flag in
  results, so an agent reads them as data.

### Disclosure

- **Network, server:** the model backends in its pool. Only when you set them up:
  - GitHub, for project issue sync;
  - your sign-in providers;
  - with the image proxy switched on, the images that chat answers show.
- **Network, CLI:** your ctx server, plus one release check against `api.github.com` in `ctx init`
  (`--no-update-check` or `CTX_NO_UPDATE_CHECK=1` skips it).
- **Stored by the server:** your blocks and blobs. A log of ctx's own model calls with their prompts and answers,
  except for content classed as credentials; the text is cleared after 90 days (`CTX_LLMLOG_RETENTION_DAYS`).
  Calls through `/v1` keep no text unless you switch that on. Web chat sessions, kept until you set
  `CTX_WEBCHAT_SESSION_RETENTION`. A log of block reads.
- **Background work, server:** embeddings and duplicate checks for new blocks; on days with new activity, a daily
  report at 03:00 per tenant, written by the chat model and saved as a block; Dream Mode, once you switch it on.
- **Stored by the CLI:** a config file with the server address and your key, in `$XDG_CONFIG_HOME/ctx`,
  `%APPDATA%\ctx` on Windows, else `~/.config/ctx`. With `ctx init`, the hooks and the statusline in Claude Code's
  `settings.json`, with a backup copy before each change. With `ctx project init`, a repository key in
  `projects/` of that folder and a `.ctx-project` file without secrets in the repository. `ctx persist --hook`
  adds session id, host name, project and agent to the metadata of each block it saves.
- **Commands, CLI:** `git`, read-only, when `ctx project` detects a repository.
- **Inbound:** HTTP on port 8080 inside the container: `/api/…`, `/mcp`, the sign-in and OAuth routes, `/health`,
  the web UI, `/v1` and `/v2` once the OpenAI-compatible API is on, and a signed webhook route for GitHub sync.

The full picture is in [Security](docs/security.md); reporting a vulnerability is in [SECURITY.md](SECURITY.md).

## More

| Document | What is in it |
|---|---|
| [Architecture](docs/architecture.md) | Retrieval pipeline, cyclic time model, Dream Mode, Guard, block types, backend pool |
| [Multi-tenancy](docs/multi-tenancy.md) | Tenants, scopes and blocks, grants, admin tiers, per-tenant settings, secrets and quotas, onboarding |
| [HTTP API](docs/api.md) | Endpoints, manage actions, graph, settings, backend pool, web chat, the OpenAI-compatible API |
| [MCP reference](docs/mcp.md) | All 16 MCP tools with parameters, answers, error codes and staged writes |
| [CLI reference](docs/cli.md) | The `ctx` commands, exit codes, tenants, quotas, keys, block grants, model consumers |
| [Security](docs/security.md) | Admin tier, sealed secrets and break-glass, trust and sensitivity, audit trail |
| [Operations](docs/operations.md) | Setup, environment variables, backends, models and hardware, backups, deploy and migrations |
| [Development](docs/development.md) | Building, platforms, the web UI, tests, gates, git hooks |
| [ctx as agent memory](docs/ctx-as-agent-memory.md) | How an agent should treat the store, and what goes wrong when it treats it as a tool |
| [Using ctx effectively](docs/using-ctx-effectively.md) | Why stored memory drifts, agent discipline, calibration |
| [Contributing](CONTRIBUTING.md) · [Governance](GOVERNANCE.md) | Pull requests, coding standards, roles, how `root` changes, releases |

## Built with AI agents

ctx is built by AI agents working against a published calibration: a map of 22 ways LLMs fail at long-running work
(memory drift, certainty without a source, caution tuned for an average user), each with real examples. It is the
method behind this project: **[gottz.de/warnings.md](https://gottz.de/warnings.md)**. Background in
[Using ctx effectively](docs/using-ctx-effectively.md).

## Sponsoring

ctx makes its retrieval decisions on measurements from its own corpus, not on vendor benchmarks. The question of
the vector index engine (pgvector against pgvectorscale, VectorChord, pgContext, or a binary-quantization path of
its own in Go) only moves on evidence. The current engine is measured at 100k vectors. Testing the alternatives
takes benchmark runs at 1M to 10M vectors: index builds in the tens of gigabytes, hours of exact ground-truth
computation per run, dedicated hardware. Those benchmarks depend on funding.

If you want those numbers to exist: [GitHub Sponsors](https://github.com/sponsors/GottZ) · [Ko-fi](https://ko-fi.com/gottz) · ETH via ENS [gottz.eth](https://app.ens.domains/gottz.eth)

## Star History

<a href="https://www.star-history.com/?repos=GottZ%2Fctx&type=timeline&logscale=&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=GottZ/ctx&type=timeline&theme=dark&logscale&legend=top-left&sealed_token=Yy6khpQuejujUPJlxutS6W7OyHG3LK2CpjPXjsF0sQ3SsC1jGc5FX2A47LX6AZ-q_Q55svVbCFEOHkRydCi0qBcF3Qq3xpc-bKPf7GLoabu0VfXiTlHBsw" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=GottZ/ctx&type=timeline&logscale&legend=top-left&sealed_token=Yy6khpQuejujUPJlxutS6W7OyHG3LK2CpjPXjsF0sQ3SsC1jGc5FX2A47LX6AZ-q_Q55svVbCFEOHkRydCi0qBcF3Qq3xpc-bKPf7GLoabu0VfXiTlHBsw" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=GottZ/ctx&type=timeline&logscale&legend=top-left&sealed_token=Yy6khpQuejujUPJlxutS6W7OyHG3LK2CpjPXjsF0sQ3SsC1jGc5FX2A47LX6AZ-q_Q55svVbCFEOHkRydCi0qBcF3Qq3xpc-bKPf7GLoabu0VfXiTlHBsw" />
 </picture>
</a>

## License

[MPL-2.0](LICENSE). By [GottZ](https://github.com/GottZ).
