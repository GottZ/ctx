# Safety and disclosure

[← ctx_handover](../README.md)

What a paired device may do, what leaves this machine, and the exact list of what the plugin reads, writes, runs and sends.

## Safety in plain words

- **What a paired device may do.** With the *pull* right (to take) it can take any session of this Hermes profile, with
  its files. With the *push* right (to send) it can create sessions here and write project copies into the allowed
  folders (`allow_roots`, by default `~/hermes-handover`), and outside them only where a project returns to the folder it came
  from or into a folder the same device delivered into before — never into Hermes' configuration, nor into places that hold
  credentials, shell or autostart settings, or git and ssh configuration. It can start a turn here only when you switch
  on `allow_remote_continue`, and only while approvals are on as far as the plugin can tell: a gateway that gets
  `HERMES_YOLO_MODE` only from its environment is not always seen ([Limits](limits.md), *Continuation and approvals*).
- **What travels as it is.** The conversation, tool outputs, attached images and files go to the other machine
  unredacted: nothing is blacked out. Whatever the agent saw, the other machine gets. Committed git history travels with
  a project, including secret files that were committed. A handover file is not encrypted. If your project encrypts
  files with a git filter that needs environment variables (for example sops), such a file can travel unencrypted — see
  [Limits](limits.md).
- **What stays here.** `.env` files, keys and credential folders stay here unless you name each one with
  `--include-secret <path>`; a general *yes* never sends them. Hermes' own credentials never leave.
- **The stored sign-in opens the whole dashboard.** The dashboard token (or user name and password) that the laptop
  stores for the server opens the server's whole dashboard — its settings, its secrets and its terminal — not only this
  plugin. Whoever gets the laptop's `.env` file gets that access. A reverse proxy in front of the server that passes
  only `/api/plugins/ctx_handover/` limits it. If you lose the laptop, run `/handover peers revoke laptop` on the server
  and change the server's dashboard token (a new `HERMES_DASHBOARD_SESSION_TOKEN` value, then restart as in [Set up](setup.md),
  step 4 — or a new dashboard password).
- **A pairing line is a password for 10 minutes** (by default). Keep it to yourself until it is used or has expired.
- **Network.** The plugin talks only to the machines you pair, and only while you pair, check (`doctor`), look at the
  status or hand over. Each handover tells the other machine facts about this one (system, host and user name, folders,
  versions, free disk space — never the values of environment variables). No other services, no telemetry, no update
  checks.
- **Your AI model is asked once per handover.** By default the plugin asks a model you already use which files the work
  needs. Either it asks the session's own model, which then reads the whole conversation once, as the session does
  anyway; or it sends up to 60,000 characters of the recent conversation, with secrets blacked out, to your main model.
  You pay for that call like for any other model call. Setting `refine` to `off` (Settings → Plugins → ctx_handover,
  *Ask a model what else the work needs (files, tools)*) stops it.

The exact list of what the plugin reads, writes, runs and sends is under [Disclosure](#disclosure) below.

## What the lock on the source guarantees

After a handover the source session is (1) ended with reason `handed_over`, (2) archived, (3) hidden and (4)
renamed to `<title> (handed over to <peer> <date>)` across its whole compression lineage — all with Hermes' own
session store, so these four marks hold even when the plugin is disabled, not loaded (safe mode) or broken. The
canonical *Bot Chat* of Bot Mode is not renamed (Hermes lets nothing rename it) and loses its title while it is handed
over: archiving releases the Bot Chat name in Hermes, so it is untitled, archived and hidden (marks (1)–(3) hold).
A reclaim or a cancelled handover gives the name back; if Bot Mode made a new Bot Chat meanwhile, it brings the old
one back into the session list as `Bot Chat (before handover)`. (5) While the plugin is loaded, every new turn and every tool call on any session of
that lineage is refused, also after a compaction rotation; each refused message leaves a placeholder that
`/handover reclaim` rewinds. The first message after Hermes starts waits up to 5 seconds for the plugin's ledger;
if the ledger cannot be opened (Hermes logs a warning), the plugin tries again with growing pauses of at most a
minute, and turns in between are not refused — marks (1)–(4) still hold. If the ledger cannot be read when a Hermes
process starts (another process holds its lock for more than 5 s), turns in that process are not checked against the
source lock until a read succeeds (the plugin keeps trying); the plugin logs the start and the end of that window.

What is **not** guaranteed: someone who opens the archived session from the archive view while the plugin is
disabled can continue it; the two copies are then independent (no merge). `/handover reclaim` takes a session
back here; the copy on the other gateway stays active.

## Disclosure

**Network:** HTTPS to the gateways you pair, only during pairing, `doctor`, status and handovers you start or
claim (plain HTTP only on loopback or for a peer you mark `--allow-insecure`); a Basic-login peer gets a password
login on its dashboard; `/handover setup` reads this gateway's own `/api/auth/providers` once. `doctor` sends one
signed hello per paired gateway; only `doctor --measure` (local CLI, TUI or Desktop) uploads zero-filled test
bodies of 1, 2, 4 and 8 MiB — up to 15 MiB per paired gateway — and remembers the largest size that passed. Each
handover sends the other side this machine's environment facts: OS and architecture, host name, user name, home
folder, Hermes home and profile name, shell, locale, time zone, Hermes and Python versions, model and provider,
whether it runs in WSL or a container, tool versions and paths, the workspace root and allowed roots with their free
disk space — never values of environment variables. No third-party services,
no telemetry, no update checks.

**Inbound:** once a peer is paired or a pairing code is pending, Ed25519-signed routes under
`/api/plugins/ctx_handover/v1/` behind the dashboard sign-in. A peer with the pull right can list and take this
profile's sessions with their working files; a peer with the push right can create sessions and write workspace
copies here, and — only if you enable `allow_remote_continue` — start one continuation turn.

**Reads:** the handed-over session through Hermes' session store, plus read-only SQL on `session_admissions` and
`async_delegations` to detect pending work; the session's working directory including uncommitted git state;
files under your home that the transcript references (asked before inclusion unless `extra_paths: auto`);
`cache/spillover`, `hook_outputs`, `native-inputs` and `cache/terminal-output` files of that session;
`processes.json` and `cron/jobs.json` (named in the note, never moved); the peer credentials through Hermes'
`get_secret` (while pairing also those of the other peers, to compare their values); the identity key
`CTX_HANDOVER_IDENTITY_KEY` through `get_secret`, and the profile `.env` file itself only when `get_secret` returns no
key or one that does not match the public key in the ledger (a process started before the key was created or
rotated); whether the provider of the session's model has a key here (its key variables through `get_secret`, else
from the profile `.env` — only whether one is set; the value is neither used nor sent); the file mode of the profile
`.env` (`doctor`); the home folders of this host's accounts from the password database (`pwd`), once per process,
for the destination denylist under **Rights**, and for the same list the home of the owner of each folder on the way
to a destination from the name service (`getpwuid`); when a file to send has more than one link, the names and file
identities — not the contents — of the entries in the credential places of the hard-link check under **Unredacted**
(at most 4096 per scan); `/etc/os-release` (OS name and version for the environment facts); on Linux the boot id
and the own PID-namespace link under `/proc` (owner stamps for the turn lease and for handover jobs, so a dead
process on this host is recognised at once) and `/proc/self/status` (the process umask for the modes of received
files); before a
continuation starts and in `doctor`, the identity of the gateway that runs this profile's turns through Hermes' own
`gateway.status` (`gateway.pid` and `gateway_state.json` of this profile and of the Hermes root, where one gateway may
serve every profile; the gateway's command line and start time) — to see a `--yolo` on its command line, never its
environment.

**Writes:** on the target — a new session, workspace copies under `allow_roots` (default: `~/hermes-handover`) or,
outside it, in a repository returning to the folder this machine handed it away from, a `--dest`/`--map` typed here,
and a folder the same peer delivered into here before (a later handover from that peer may write there again), never
over uncommitted local work except an unchanged copy this plugin handed away earlier, and the session's cache files
under HERMES_HOME; on the source — ends, archives, hides and renames the handed-over session, pauses its
goal/heartbeat/loop and blocks new turns on it (each blocked message leaves a placeholder that `/handover reclaim`
rewinds; reclaim restores everything else too); `.env` gets `CTX_HANDOVER_IDENTITY_KEY`, the peer token/password you
enter in the plugin settings form and, at each pairing, a copy of that credential and of the proxy header values for
that peer alone (`CTX_HANDOVER_PEER_TOKEN_<peer id>`, `CTX_HANDOVER_PEER_PASSWORD_<peer id>`,
`CTX_HANDOVER_PEER_HEADER<n>_<peer id>`, through Hermes' own `.env` writer and read back; `/handover peers remove`
blanks that peer's variables the same way before it drops the entry — and the form's token or password field when it
still holds that peer's value and no other paired gateway uses it, so the next pairing cannot carry it elsewhere; a
value still there afterwards keeps the entry and the reply names the variable; a form field with another value and
variables you named in `peer_extra_headers` stay as they are); `config.yaml` gets
`plugins.entries.ctx_handover.*`, and only if you accept ctx recording in `/handover setup` does that command add the
ctx MCP server to `plugins.entries.ctx_handover.mcp_allowlist` (through Hermes' own config writer, merged with what is
there; on a package-managed install it prints the `hermes config set` command instead); the plugin data folder
`<HERMES_HOME>/plugin-data/ctx_handover/` holds the ledger, staging, the block cache, reports and logs.

**Commands:** `git` without a shell, with hooks, credential helpers, fsmonitor, submodule recursion and LFS smudge
disabled for received data (only the one `git lfs checkout` call gets the standard Git LFS filter, see [Use in detail](usage.md#use-in-detail)); git, and what git starts (git-lfs, helpers), gets an environment built from an
allowlist — the search path, the home folder, `XDG_CONFIG_HOME`, the temp folder, the time zone, `SUDO_UID` (git
needs it when Hermes runs as root through sudo on another user's repository) and on Windows the system variables git
needs to start — and no other variable of the Hermes process, so provider keys and tokens never reach it (no git
call talks to a remote, so no SSH agent socket, proxy or CA setting either; for a filter of your own git
configuration that needs more, see [Limits](limits.md)); `<tool> --version` for
a fixed list of 30 developer tools plus those you name in the `tool_probe` setting, each run with only the search
path (and the Windows system variables) of this process and a home and temp folder of its own in a scratch folder in
the system temp directory (`ctxh-probe-*`, removed afterwards); with `--continue`,
one detached `hermes chat --resume <id>` single-query run on the target (Hermes' single-query approval policy
applies, default deny), started with this gateway's environment — the turn needs the model provider's keys — minus
the auto-approve variables, and never when
approvals are off there as far as the starting process can tell (see [Limits](limits.md)), nor when that process cannot identify
the gateway that runs the turn.

**Background work:** daemon threads, by name — a worker per handover (`ctxh-job-<job>`) with parallel block
transfers (`ctxh-move`) and a lease and owner refresher during long steps (`ctxh-keepalive`); for operations another
gateway started here one thread each (`ctxh-op-<op>`) and their progress beat (`ctxh-op-beat`); a ledger refresher for
the turn veto (`ctx_handover-veto`); the hooks' start, which opens the ledger and the veto index once per process
(`ctxh-hook-runtime`; a turn waits for it at most 5 seconds); a janitor that resumes or cleans up interrupted handovers
(before each handover, and once per process — on the `ctxh-janitor` thread when the first hook starts it); a
`ctxh-maintenance` thread that suspends idle jobs of other devices; one writer thread for note delivery state
(`ctx_handover-notes`); the notice to the other gateway after a reclaim (`ctxh-reclaimed-<job>`); a watcher per
continuation run that records its exit (`ctx_handover-continue-<job>`); a handover the agent asked for
(`ctx_handover-agent-request`); the `refine` model call under its time limit (`ctx_handover-refine`); the tool version
probes (`ctxh-probe`); a timer per git call that ends a hung git; and, with ctx, a `ctxh-outbox` thread that sends the
ctx records. Staging and a block cache in the plugin data folder (default up to 5 GiB, 3 days; `/handover gc`). **Retention:** a source's staging goes
when its handover ends (leftovers with the janitor or `/handover gc`); reports, offers 30 days after they expired, sent
ctx records and the backups of sessions taken back here go after 30 days (a session still handed over keeps its backup
for `/handover reclaim`); `/handover gc --job` removes everything of one finished job. While a handover runs, the cache
can grow beyond `block_cache_gb` by the blocks stored during at most the last 24 h, because the GC spares everything a
running handover may still need; the first GC after the handover ends brings it back under the limit.

**Stored secrets:** an Ed25519 private key in `.env`; paired peers' public keys and short-lived pairing codes
(≤ 10 min by default, at most 60) in the plugin data folder (0700/0600). Bundles contain the raw transcript and
workspace unencrypted (0600).

**Model use (on by default, `refine: off` disables it):** once per handover the plugin asks for resources the file
scan missed — as a side question to the session's own model (with the `llm.side_question` grant; this sends the
whole session context once, so it is skipped above 200 000 tokens) or as up to 60,000 characters of redacted
recent transcript to your main model. Inference costs are yours. `refine: off` stops these calls; an offer made
in a gateway (`/handover to <peer>`) keeps the answer it already got and passes it on.

**ctx (optional):** with ctx_checkpoint as memory provider and your consent in `/handover setup`, one redacted
record per side of each handover through Hermes' MCP dispatch, with a `search` for the checkpoint head before each
`store` (to link the record to it); a side finished without ctx (e.g. offline) is recorded later. A long session id,
or one with other characters (such as Hermes' 70-character `local-<sha256>` ids), is shortened for that search the way
ctx_checkpoint shortens it in its titles (cut, with a hash of the full id), so the record finds the head for every
id. ctx is never
needed for a handover, a ctx failure never stops one, and a job start waits at most 2 seconds for ctx (the records
go out on a thread of their own).

**Core modules used:** `hermes_state`, `hermes_constants`, `hermes_cli.config`, `agent.secret_scope`,
`agent.plugin_llm`, `agent.redact`, `plugins.plugin_storage`, `gateway.platforms.base`, `gateway.status`,
`providers`, and the
`PluginContext` Hermes hands to `register()`. No runtime overrides, no self-updater.

**What the peer credential means:** the dashboard token or Basic login you store for a peer opens that peer's
whole dashboard (configuration, secrets, terminal) — not only this plugin; a stolen `.env` gives full access there.
A stored credential goes only to the address it was stored for (scheme, host and port): a pairing whose address
differs is refused before anything is sent, unless you enter that address's own credential in the form. The first
request of a pairing carries no credential; behind a dashboard sign-in it follows the gate's refusal, and the other
side's key can only be checked once the plugin behind that gate answers — so a credential freshly typed into the
form goes to the address in the pairing line you paste: check that address before you paste it.
Prefer a reverse proxy that only passes `/api/plugins/ctx_handover/` and checks its own header
(`peer_extra_headers`); `/handover doctor` warns while such a credential is stored and when `.env` is readable by
other users; `/handover peers revoke <device>` on the server removes a lost device's key at once (changing the
dashboard token is then your step).

**Rights:** `pull` lets the peer take sessions of this profile with the files of their own plan and of
`export_roots`; `push` lets it create sessions and workspace copies under `allow_roots` (default
`~/hermes-handover`), a returning repository in the folder this machine handed it away from and a folder it
delivered into here before, never in Hermes,
credential, shell-startup, autostart, git or ssh configuration locations. No destination — not even one typed here —
lies in those locations of any account's home (yours, the homes of the password database and of the owners of the
folders on the way, `/home/*`, `/Users/*`, `/var/home/*`, `/root`, and the Windows homes behind WSL's drive mounts,
`/mnt/<drive>/Users/*` and `/<drive>/Users/*`), in a home itself or above one, or in the system places (`/etc`, `/usr`,
`/bin`, `/var/spool`, `/Library`, `/System`, `/opt/homebrew` and more; on Windows `Windows`, `Program Files`,
`Program Files (x86)`, `ProgramData`, and the same folders of each WSL drive); on Windows and on those WSL drives also
not in the PowerShell profile folders (in `Documents` and in OneDrive's documents folder, also under its localised
name such as `OneDrive/Dokumente`) or `WindowsApps`, and `~/Documents` itself is no destination there. A home whose
owner the name service does not know, a WSL drive mounted elsewhere than `/mnt/<drive>` or `/<drive>`, and a OneDrive
documents folder named in a language outside the plugin's list (`policy.ONEDRIVE_DOCUMENTS_NAMES`) are not
recognised. A bundle that no paired, non-revoked key signed is
refused by `import` unless you add `import --trust-unsigned`; such a session arrives marked as from an "unverified
handover file", never starts a continuation and binds no return trip to its exporter. Its instruction files ask for a
confirmation.

**Unredacted:** transcripts, spillover, hook-output and terminal-output files and attached images go to the peer as
they are; committed history travels
with a repository, including tracked secret files (the plan warns); workspace `.env` files only with
`--include-secret` per path on the source; Hermes' own credentials never. The content sniff reads the first 8 KiB
of each candidate file. A hard link to a credential file the plugin knows (in your home's secrets folders such as
`~/.ssh`, `~/.aws` and the browser profiles, a file with a secret name at the top of your home or in `~/.config` or one
folder below it, Hermes' credential stores) is left out as that file would be: `--include-secret` on its path releases
one of your home, nothing releases Hermes'. A hard link to a credential file elsewhere (another account's home, any
other place) is caught only by the name and content checks. The check reads at most 4096 entries of the credential
places per scan: first the fixed files (Hermes' credential stores, the names at the top of your home and of
`~/.config`), then the folders level by level, browser profiles last. When it cannot read them all (a large
`~/.kube/cache` or browser profile), every file with more than one link it could not compare is left out as well — as
a secret (`--include-secret` on its path releases it) once all of Hermes' places were read, otherwise like Hermes' own
files (nothing releases it). The block cache keeps copies up to 3 days / 5 GiB (`/handover gc --all`).
With `allow_remote_continue` or `allow_agent_start`, a turn runs without a person present (single-query approval
policy; refused when approvals are off as far as the starting process can tell), and refused when that process
cannot identify the gateway that runs it. Under `plugins.isolation: host` the core buffers request bodies before
the plugin's size limit applies. The plugin never updates itself and never writes into Hermes plugin, skill,
memory or config directories other than its own entry `plugins.entries.ctx_handover`. Administrative commands
run only in the local CLI, TUI or Desktop.

**Not moved:** running processes, subagents, cron jobs. **Limits:** gateways reachable only through Desktop's SSH,
OAuth/Nous or Hermes-Cloud connections need the bundle path in this version; images attached before the handover
replay only when the absolute path is identical; disabling the plugin lifts the turn veto on handed-over sessions
(they stay ended, archived, hidden and renamed).
