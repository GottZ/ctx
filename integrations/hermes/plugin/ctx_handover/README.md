# ctx_handover — move a live Hermes session to another gateway

Works without ctx. `ctx_handover` moves a live [Hermes](https://github.com/NousResearch/hermes-agent) session and
the working files it needs to another Hermes gateway you own — laptop ↔ server by pull, push or an offline bundle
— and can continue it there: the transcript is transferred byte-exact and verified by hash, git repositories keep
branches, stash and uncommitted work, and the continuing agent gets a note about what changed.

Not the core `/handoff` (session → a messaging app), not hermes-handoff (summary handoff within one profile), not
session-relay (fresh session on the same gateway).

## Limits — read this first

- **Which connections work online.** The device that starts a handover (the *dialer*, usually the laptop) talks
  to the other gateway's dashboard over HTTP(S) with a credential this plugin can hold:
  - a gateway or `hermes serve` that runs with a fixed dashboard token (`HERMES_DASHBOARD_SESSION_TOKEN`) —
    gate mode `session_token` (default) or `bearer`;
  - a `hermes serve` / standalone dashboard with the Basic login provider — gate mode `basic_login`
    (user name and password; not the gateway *Webapp*, which accepts only native-app sign-ins there — use its
    session token instead);
  - a reverse proxy in front of either, with extra headers of its own (`peer_extra_headers`).

  **Gateways that Hermes Desktop reaches only through an SSH tunnel, an OAuth/Nous sign-in or Hermes Cloud have no
  credential a plugin can hold. For them this version offers the bundle path only** (`/handover export` → copy the
  file → `/handover import`). A Desktop-side conductor that uses Desktop's own connections is planned, not built.
- The laptop always dials the server (works behind NAT). A gateway that nobody can dial (a home machine behind NAT
  while the laptop sleeps) is reached by bundle only.
- **Not moved:** running processes, subagents/delegations and cron jobs. The note on the other side names them,
  with the command to pause a cron job that is still attached to the old session.
- Two copies that both went on are never merged.
- No Desktop UI of its own yet: everything runs through `/handover …` in any Hermes chat (CLI, TUI, Desktop) and
  `hermes handover …` in a terminal.
- Workspaces of remote terminal backends (docker, ssh, modal …) are not sources; the plugin moves files of the
  machine the gateway runs on.
- Images attached before the handover replay only when their absolute path is identical on the other machine.
- `git` 2.38 or newer is needed on both sides for repositories (`/handover doctor` checks it).
- Hermes 0.22.6 or newer. Tested on Linux; the macOS and Windows code paths exist but have no end-to-end runs yet.
- A message typed into the paused source while a long transfer runs is not answered there: its turn waits at most
  30 minutes for the session (then Hermes reports it as not processed) and otherwise meets the lock. When the chat
  stored the message on submit (TUI and Desktop do), it travels along as the session's last message, and the note
  on the other side says it is waiting there.
- Under `plugins.isolation: host` there is no `hermes handover` CLI (use the slash form), no progress notice
  (`/handover status` shows progress), and Hermes buffers request bodies before this plugin's size limit applies —
  prefer `in_process` or a proxy body limit on a receiving gateway.
- **Sessions from older Hermes builds.** Messages written before Hermes stamped message and tool-call identities
  get stable identities derived from the original rows during the transfer; the source session is not modified.
  If Hermes backfills those identities on the source while a handover is running, the target refuses the late
  changes (`verify_failed`) and the handover starts over from a fresh export — nothing is accepted silently.
- **Messages typed into a handed-over session.** While ctx_handover is loaded, every message sent in the
  handed-over session is withheld (no model call) and answered with a notice where the session continues.
  `/handover reclaim` removes these attempts from the end of the conversation and lists their text. Attempts that
  a context compaction moved into an earlier part of the session stay in its history, because Hermes allows no
  changes there; reclaim still lists their text and says how many remain.
- **Display after an undone message.** If a message that a compaction carried forward was later undone on the
  source, the target's chat display shows the earlier copy of that message again. What the model sees is
  identical on both sides; undone messages themselves are not transferred.
- **Pulling from a machine that goes quiet.** When a pull stops talking to the source for
  `participant_idle_timeout` (15 minutes), the source releases its locks and the session there is usable again;
  `/handover resume` locks it again and continues.
- **A git command that died mid-way.** A git process killed in the middle of a command in a destination
  repository (power loss, killed process) can leave `.git/index.lock` behind. The plugin never removes it (it may
  belong to another git process): the step that resumes the handover refuses that repository and the handover
  ends marked `needs_attention`. Remove the lock once no git process runs there, then hand over again.

## What the lock on the source guarantees

After a handover the source session is (1) ended with reason `handed_over`, (2) archived, (3) hidden and (4)
renamed to `<title> (handed over to <peer> <date>)` across its whole compression lineage — all with Hermes' own
session store, so these four marks hold even when the plugin is disabled, not loaded (safe mode) or broken. (5)
While the plugin is loaded, every new turn and every tool call on any session of that lineage is refused, also
after a compaction rotation; each refused message leaves a placeholder that `/handover reclaim` rewinds.

What is **not** guaranteed: someone who opens the archived session from the archive view while the plugin is
disabled can continue it; the two copies are then independent (no merge). `/handover reclaim` takes a session
back here; the copy on the other gateway stays active.

## Set up — no terminal needed

1. **Install and enable** the plugin on both machines (Hermes Desktop → Capabilities → Plugins, once for the
   server's gateway and once for *This device*). On the gateway that should receive handovers, restart
   `hermes serve` and the gateway afterwards, otherwise its routes are not mounted (the dialer then reports
   "has no ctx_handover routes").
2. **On the receiving gateway** (say *server*): open Settings → Plugins → ctx_handover and fill *Address other
   devices use to reach this gateway* (`listen_url`), e.g. `https://server.example.net`. Run `/handover setup`
   in a chat on that gateway: it tells you whether its dashboard uses a fixed token or a Basic login and which
   field the other device needs.
3. **On the dialing device** (say *laptop*): Settings → Plugins → ctx_handover → *Dashboard token of the other
   gateway* (or user name and password for `basic_login`). Hermes stores secrets in this profile's `.env`
   (`CTX_HANDOVER_PEER_TOKEN`, `CTX_HANDOVER_PEER_PASSWORD`), never in `config.yaml`.
4. **Pair:** in a chat on the server run `/handover pair --name laptop`. Copy the single line that starts with
   `ctxh-pair:` and run `/handover pair ctxh-pair:…` in a chat on the laptop. The line carries the address, a
   one-time code (10 minutes, single use) and the server's key fingerprint, which the laptop checks itself — it
   never carries a dashboard credential. For a plain-HTTP address that only your VPN reaches (e.g. Tailscale) add
   `--allow-insecure`. The laptop names the server as the line does, or takes the next free name (`server-2`) when
   that one is taken there; with `--name` you choose it, and a taken name is refused before the code is used.
5. `/handover doctor` on both sides checks the setup and says what to fix. `/handover doctor --measure` also finds
   the request-body limit of a proxy in front of each paired gateway (test uploads, see Disclosure).

## Use

| Situation | Where | Command |
|---|---|---|
| Take the server session to the laptop before a flight | laptop chat | `/handover take` (picks the only paired gateway; shows offers and the 20 most recent sessions to choose from) |
| …or start it from the server session | server chat | `/handover to laptop` — leaves an offer (2 h); then `/handover take` on the laptop |
| Back to the server after landing | the laptop session | `/handover to server` — repositories return into their original folders when nothing changed there meanwhile |
| Before closing time, let the agent keep working | the session | `/handover to server --continue "run the tests and fix what fails"` (needs `allow_remote_continue` on the server) |
| A turn is still running | the session | add `--wait` to hand over as soon as it finishes |
| No connection | source / target | `/handover export` writes one `.ctxh.tar`; `/handover import <file>` on the other machine (`export --keep` leaves this session active and marks the bundle as a copy; status and the final message say so) |
| Take it back here | the source session | `/handover reclaim` (`--force` while a handover is being committed) |

Before anything is locked the plugin shows what would move. It asks for a confirmation when items are only
proposed, a destination has local changes (a fresh folder is used), the handover is larger than
`confirm_above_mb`, a secret is included with `--include-secret`, the model there does not work offline or the
session is larger than its context window, items outside the project are included, instruction files arrive
from an unverified source, or the session's working folder is left out by the policy (for example because it lies
inside the Hermes home; the summary names the folder, and the project does not come along). The choices are *yes*,
*yes without the proposed items*, *show the plan and stop*, *cancel*; `--yes` skips the question. `--yes` does not
confirm a left-out working folder: a person answers that question (without an answer nothing is locked), or
`--no-workspace` hands the session over without its folder on purpose. Files that look like secrets are never sent
by a blanket yes (neither `--yes` nor the *yes* choice takes them along); name each one with
`--include-secret <path>`. An unanswered question (no reply within Hermes' question timeout
`agent.clarify_timeout`, 90 s by default) continues without the proposed items when those are the only reason and
cancels otherwise — nothing is locked in either case.

The final message names the session on the other side, its folder, the model and whether it works offline, and
what to do next; after a pull it also lists paths you asked for with `--include` that the other side does not
release (withheld by source policy — a pull's own hints only narrow what the source offers). On the receiving side
`/handover status <job>` shows the note the agent gets with your first message there; the note is not shown in
the transcript view.

All commands: `/handover to | take | export | import | status | peers | pair | reclaim | cancel | resume | setup |
doctor | identity | gc | logs | help` — `/handover help <command>` shows the options. `hermes handover <same>` runs
them in a terminal (`--json` for scripts, `--session <id>` names the session; exit codes 0 ok, 1 failed, 2 usage,
3 busy/conflict, 4 auth/gate, 5 in-doubt/suspended). Administrative and data-moving commands (`pair`, `setup`,
`peers`, `identity`, `take`, `to`, `export`, `import`, `reclaim`, `resume`, `gc`, `cancel`) answer only in the
local CLI, TUI or Desktop; on messaging platforms they are refused.

The agent has a `handover` tool: it can show the plan, name resources the file scan cannot see (paths, tools and
the names — never values — of environment variables; anything outside the project is only proposed) and ask for a
handover. It starts one itself only with `allow_agent_start`, then without proposed items and stopping at any
other confirmation.

## Settings

Settings → Plugins → ctx_handover (or `plugins.entries.ctx_handover.settings` in `config.yaml`). Every key has a
working default; the most relevant:

| Key | Default | Meaning |
|---|---|---|
| `inbound` | `auto` | accept handovers only while a device is paired or a pairing code is pending; `on`, `off` |
| `listen_url` | `""` | address put into pairing strings (else `webapp.public_url` / `dashboard.public_url`) |
| `peer_gate`, `peer_token`, `peer_username`, `peer_password`, `peer_extra_headers` | `session_token` | how this device signs in to the other gateway's dashboard |
| `workspace_root`, `allow_roots` | `~/hermes-handover` | where received workspaces and bundles go; writes land only under `allow_roots`. When the home folder lies inside the Hermes home (the official Docker image), the default is `<HERMES_HOME>/handover-workspaces`, the one folder of the Hermes home a handover may write to; `/handover setup` shows the folder in use |
| `export_roots` | `[]` | extra paths a device with the pull right may request beyond this machine's own plan |
| `max_total_gb`, `max_file_mb`, `max_session_mb`, `confirm_above_mb` | 10, 100, 512, 500 | size limits and the confirmation threshold |
| `refine` | `auto` | ask a model once per handover which resources the work needs (`side_question`, `llm`, `off`) |
| `extra_paths` | `ask` | files outside the project that the conversation used (`auto`, `off`) |
| `model_mapping` | `auto` | keep the session's model when the receiving machine can serve it, else its default |
| `allow_remote_continue` | `false` | let a paired device start one continuation turn here |
| `allow_agent_start` | `false` | let the agent start a handover it was asked for |
| `ctx_persist`, `ctx_category` | `auto`, `handover` | ctx records (only with ctx_checkpoint and your consent) |

The full list with all limits and timeouts is the `config_schema` in `plugin.yaml`. A value of the wrong type or
outside its choices counts as that key's default (a bare YAML `off` means `off`); `/handover doctor` names each
such value.

## Disclosure

**Network:** HTTPS to the gateways you pair, only during pairing, `doctor`, status and handovers you start or
claim (plain HTTP only on loopback or for a peer you mark `--allow-insecure`); a Basic-login peer gets a password
login on its dashboard; `/handover setup` reads this gateway's own `/api/auth/providers` once. `doctor` sends one
signed hello per paired gateway; only `doctor --measure` (local CLI, TUI or Desktop) uploads zero-filled test
bodies of 1, 2, 4 and 8 MiB — up to 15 MiB per paired gateway — and remembers the largest size that passed. No
third-party services, no telemetry, no update checks.

**Inbound:** once a peer is paired or a pairing code is pending, Ed25519-signed routes under
`/api/plugins/ctx_handover/v1/` behind the dashboard sign-in. A peer with the pull right can list and take this
profile's sessions with their working files; a peer with the push right can create sessions and write workspace
copies here, and — only if you enable `allow_remote_continue` — start one continuation turn.

**Reads:** the handed-over session through Hermes' session store, plus read-only SQL on `session_admissions` and
`async_delegations` to detect pending work; the session's working directory including uncommitted git state;
files under your home that the transcript references (asked before inclusion unless `extra_paths: auto`);
`cache/spillover`, `hook_outputs`, `native-inputs` files of that session; `processes.json` and `cron/jobs.json`
(named in the note, never moved); the file mode of the profile `.env` (`doctor`).

**Writes:** on the target — a new session, workspace copies under `allow_roots` (default: `~/hermes-handover`),
never over uncommitted local work except an unchanged copy this plugin handed away earlier, and the session's
cache files under HERMES_HOME; on the source — ends, archives, hides and renames the handed-over session, pauses
its goal/heartbeat/loop and blocks new turns on it (each blocked message leaves a placeholder that `/handover
reclaim` rewinds; reclaim restores everything else too); `.env` gets `CTX_HANDOVER_IDENTITY_KEY` (and the peer
token/password you enter in the plugin settings form); `config.yaml` gets `plugins.entries.ctx_handover.*`, and
only if you accept ctx recording in `/handover setup` does that command add the ctx MCP server to
`plugins.entries.ctx_handover.mcp_allowlist` (through Hermes' own config writer, merged with what is there; on a
package-managed install it prints the `hermes config set` command instead); the plugin data folder
`<HERMES_HOME>/plugin-data/ctx_handover/` holds the ledger, staging, the block cache, reports and logs.

**Commands:** `git` without a shell, with hooks, credential helpers, fsmonitor, submodule recursion and LFS smudge
disabled for received data; `<tool> --version` for a fixed list of about 30 developer tools; with `--continue`,
one detached `hermes chat --resume <id>` single-query run on the target (Hermes' single-query approval policy
applies, default deny), started with auto-approve variables removed from its environment and never when
approvals are off there.

**Background work:** a worker thread per handover, a lease refresher, a ledger refresher for the turn veto, a
janitor that resumes or cleans up interrupted handovers, and one writer thread for note delivery state; staging and
a block cache in the plugin data folder (default up to 5 GiB, 3 days; `/handover gc`). While a handover runs, the
cache can grow beyond `block_cache_gb` by the blocks stored during at most the last 24 h, because the GC spares
everything a running handover may still need; the first GC after the handover ends brings it back under the limit.

**Stored secrets:** an Ed25519 private key in `.env`; paired peers' public keys and short-lived pairing codes
(≤ 10 min by default, at most 60) in the plugin data folder (0700/0600). Bundles contain the raw transcript and
workspace unencrypted (0600).

**Model use (on by default, `refine: off` disables it):** once per handover the plugin asks for resources the file
scan missed — as a side question to the session's own model (with the `llm.side_question` grant; this sends the
whole session context once, so it is skipped above 200 000 tokens) or as up to 60,000 characters of redacted
recent transcript to your main model. Inference costs are yours.

**ctx (optional):** with ctx_checkpoint as memory provider and your consent in `/handover setup`, one redacted
record per side of each handover through Hermes' MCP dispatch; a side finished without ctx (e.g. offline) is
recorded later. ctx is never needed for a handover and a ctx failure never stops one.

**Core modules used:** `hermes_state`, `hermes_constants`, `hermes_cli.config`, `agent.secret_scope`,
`agent.plugin_llm`, `agent.redact`, `plugins.plugin_storage`, `gateway.platforms.base`, `providers`, and the
`PluginContext` Hermes hands to `register()`. No runtime overrides, no self-updater.

**What the peer credential means:** the dashboard token or Basic login you store for a peer opens that peer's
whole dashboard (configuration, secrets, terminal) — not only this plugin; a stolen `.env` gives full access there.
Prefer a reverse proxy that only passes `/api/plugins/ctx_handover/` and checks its own header
(`peer_extra_headers`); `/handover doctor` warns while such a credential is stored and when `.env` is readable by
other users; `/handover peers revoke <device>` on the server removes a lost device's key at once (changing the
dashboard token is then your step).

**Rights:** `pull` lets the peer take sessions of this profile with the files of their own plan and of
`export_roots`; `push` lets it create sessions and workspace copies under `allow_roots` (default
`~/hermes-handover`), never in Hermes, credential, shell-startup, autostart, git or ssh configuration locations.

**Unredacted:** transcripts, spillover and hook-output files go to the peer as they are; committed history travels
with a repository, including tracked secret files (the plan warns); workspace `.env` files only with
`--include-secret` per path on the source; Hermes' own credentials never. The content sniff reads the first 8 KiB
of each candidate file. The block cache keeps copies up to 3 days / 5 GiB (`/handover gc --all`).
With `allow_remote_continue` or `allow_agent_start`, a turn runs without a person present (single-query approval
policy; refused when approvals are off). Under `plugins.isolation: host` the core buffers request bodies before
the plugin's size limit applies. The plugin never updates itself and never writes into Hermes plugin, skill,
memory or config directories other than its own entry `plugins.entries.ctx_handover`. Administrative commands
run only in the local CLI, TUI or Desktop.

**Not moved:** running processes, subagents, cron jobs. **Limits:** gateways reachable only through Desktop's SSH,
OAuth/Nous or Hermes-Cloud connections need the bundle path in this version; images attached before the handover
replay only when the absolute path is identical; disabling the plugin lifts the turn veto on handed-over sessions
(they stay ended, archived, hidden and renamed).

## Troubleshooting

- **`hermes handover` or `/handover` is unknown on a Hermes installed from a source checkout.** Such an
  installation reads its own version with `git tag --merged HEAD` under a 3-second limit; on a busy machine the
  limit runs out, Hermes falls back to an older base version, and this plugin (it needs 0.22.6 or newer) is skipped
  in that process — the log says `Plugin 'ctx_handover' skipped: requires hermes >=0.22.6`, and
  `hermes plugins enable ctx_handover` can be refused the same way. Run the command again, or restart the gateway,
  once the machine is less busy. Packaged installations (Docker, Nix) carry a version stamp and are not affected.

## Tests

```bash
# unit suites (fakes, a real SessionDB and real git in temp dirs; the host contract comes from a Hermes checkout)
PYTHONPATH=/path/to/hermes-agent:integrations/hermes/plugin python -m pytest \
    integrations/hermes/plugin/ctx_handover/tests/unit -q
# lint + format + bandit rules (ruff.toml in this directory)
cd integrations/hermes/plugin/ctx_handover && ruff check . && ruff format --check .
# catalog checks
hermes plugins validate integrations/hermes/plugin/ctx_handover
```

`tests/e2e/` drives two real gateways in a lab (two `HERMES_HOME`s, a fake model); it needs that lab running and is
not part of the unit run.

## Layout

```
__init__.py          register(): /handover, hermes handover, four hooks, the handover tool — no I/O
plugin.yaml, VERSION manifest (settings form = config_schema) and the single version source
engine/              the engine; it runs the same in the gateway, hermes serve, the CLI and the plugin host
  commands.py        /handover and hermes handover, hooks, composition with the engine
  surfaces.py        grammar, settings, confirmation, final and error texts
  progress.py        progress notice (post_message), terminal progress
  agent_tool.py      the handover tool
  continuation.py    the detached continuation run
  ctxrecords.py      optional ctx records (outbox)
  …                  session, planning, workspace, transport and orchestration modules
dashboard/           the signed /api/plugins/ctx_handover/v1 routes
tests/               unit, integration and lab suites
```

## License

MPL-2.0, part of [ctx](https://github.com/GottZ/ctx).
