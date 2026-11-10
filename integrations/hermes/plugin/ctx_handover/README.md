# ctx_handover — take a running Hermes session to another machine

Close your laptop after work and let the session go on on your server. `ctx_handover` moves a running
[Hermes](https://github.com/NousResearch/hermes-agent) session — the conversation and the project folder it works in
— from one of your machines to another, and back again. On the other machine you open the session and carry on, or
you give the agent one more task that it works on there while your laptop stays closed.

It works between two machines that run Hermes and that you own; this README calls them *your laptop* and *your
server*. Works without ctx. Not the core `/handoff` (Hermes' own command that sends a session to a messaging app),
not hermes-handoff (a summary handed on within one profile), not session-relay (a fresh session on the same machine).

## Before you start

You need:

- Hermes 0.22.6 or newer on both machines, and `git` 2.38 or newer when your project uses git.
- A way for your laptop to reach your server over the network: an address such as `https://server.example.net`, or
  a plain `http://` address that only your VPN reaches (for example Tailscale).
- The sign-in of your server's dashboard: a fixed dashboard token, or a user name and password. `/handover setup` on
  the server tells you which one.

What does not work, or only partly:

- If Hermes Desktop reaches your server only through an SSH tunnel, a Nous/OAuth sign-in or Hermes Cloud, the
  plugin has no sign-in it can use. Then only the handover file works (step 4 below).
- The laptop always connects to the server, never the other way round. A machine that nobody can reach (a home
  computer behind a router while the laptop sleeps) can only get a handover file.
- Running programs, subagents and scheduled (cron) jobs do not move. The agent on the other machine gets a note that
  names them.
- If both copies of a session go on, they are never merged.

Every limit, with the details, is listed under [Limits](#limits).

## 1. Pair your laptop and your server (once)

Pair the two machines once so they trust each other. You type the `/handover` commands below into a Hermes chat on
the machine named — in the terminal (`hermes`), the TUI or Hermes Desktop. Chats in messaging apps cannot run them.

**On both machines**

1. Install and enable the plugin: in Hermes Desktop under Capabilities → Plugins, or in a terminal:

   ```
   hermes plugins install ctx_handover
   hermes plugins enable ctx_handover
   ```

**On your server**

2. Tell the plugin the address your laptop uses to reach the server. In Settings → Plugins → ctx_handover, fill
   *Address other devices use to reach this gateway* (`listen_url`), or run in a terminal:

   ```
   hermes config set plugins.entries.ctx_handover.settings.listen_url https://server.example.net
   ```

3. Restart Hermes on the server, so that it answers the laptop: restart the gateway — the Hermes service that runs
   your chats on that machine — with `hermes gateway restart`, and restart `hermes serve` too if you run it. Without
   the restart, the laptop later reports that the server "has no ctx_handover routes".
4. In a chat on the server, run:

   ```
   /handover setup
   ```

   It checks the address and tells you how the server's dashboard signs people in:
   - *with a fixed token*: set `HERMES_DASHBOARD_SESSION_TOKEN` on the server (then restart `hermes serve` and the
     gateway). Your laptop needs this same token in step 5.
   - *with a Basic login*: your laptop needs the dashboard's user name and password in step 5.

**On your laptop**

5. In Settings → Plugins → ctx_handover, enter the server's sign-in: *Dashboard token of the other gateway*
   (`peer_token`). For a Basic login, set *How to pass the other gateway's dashboard sign-in* (`peer_gate`) to
   `basic_login` and fill in *Dashboard user name* and *Dashboard password*. Hermes keeps the token and the password
   in this profile's `.env` file, never in `config.yaml`.

**On your server**

6. Create a pairing line:

   ```
   /handover pair --name laptop
   ```

   The answer shows one line that starts with `/handover pair ctxh-pair:`. That line holds the server's address, a
   one-time code (valid for 10 minutes, usable once) and a fingerprint of the server's key, which the laptop checks.
   It never holds your dashboard sign-in. Treat it like a password until it is used.

**On your laptop**

7. Copy that whole line into a chat on the laptop and send it:

   ```
   /handover pair ctxh-pair:…
   ```

   For a plain `http://` address that only your VPN reaches, add `--allow-insecure` at the end. The answer starts
   with "Paired with" and the name the laptop gave the server — in this README, `server`. Use that name in the
   commands below; `/handover peers` lists the names.
8. Run `/handover doctor` in a chat on each machine. It checks the setup and says what to fix.

By default, pairing lets the laptop do both: take sessions from the server and send sessions to it. To allow only
one, add `--rights pull` (take) or `--rights push` (send) in step 6.

## 2. After work: send the session to your server

In the laptop session you want to move, run:

```
/handover to server
```

Before anything happens, the plugin shows what it would move: the project folder and its size, other files the
conversation used, and what it leaves out. When it asks, answer *yes*. Then it copies the conversation and the files
to the server. **Wait for the final message before you close the laptop** — the laptop does the copying. The final
message names the session on the server and its folder.

If the agent is still working on an answer, add `--wait`: the handover starts as soon as the answer is done.

Afterwards the session is locked on the laptop: Hermes marks it as ended, archives and hides it, and adds "(handed
over to server …)" to its title, so it cannot go on here by accident. On the server, the session is listed in the
Desktop sidebar and in `hermes sessions list`; `hermes --resume <session-id>` opens it.

**Let the agent keep working while the laptop is closed**

1. Once, on the server, allow a paired device to start a turn there:

   ```
   hermes config set plugins.entries.ctx_handover.settings.allow_remote_continue true
   ```

   (or switch on *Let a paired device start one continuation turn here* in Settings → Plugins → ctx_handover).
2. On the laptop, hand over with a task for the agent:

   ```
   /handover to server --continue "run the tests and fix what fails"
   ```

The server then runs one turn with that task: the agent works on it until it has answered, then the session waits
for you. Nobody is there to approve anything in that turn, so every command that would ask for your approval is
refused — Hermes' rule for such unattended turns (`approvals.single_query_mode`, `deny` by default). The turn never
starts when approvals are switched off on the server (for example a gateway started with `--yolo`), nor when the
plugin cannot find out which Hermes service on the server would run it; then the final message names the command
that starts the turn there by hand. One case the plugin cannot see is described under [Limits](#limits)
(*Continuation and approvals*). `--continue` without a task sends "Continue where we left off."

## 3. On the road: bring the session to your laptop

On the laptop, in any chat:

```
/handover take
```

It lists the sessions on the server — sessions left for you first, then the 20 most recent ones — and asks which one
to take. To take the most recently used session on the server without a question:

```
/handover take server --latest
```

The session and its project folder are copied to the laptop. The final message names the session and its folder;
open it from the Desktop sidebar or with `hermes --resume <session-id>`. On the server, the session is now locked,
as described in step 2.

A project that came from this laptop goes back into its old folder when nothing changed there meanwhile. Otherwise
it lands in a fresh folder, and the final message names it. Other projects arrive under `~/hermes-handover`
(setting `workspace_root`).

You can also start on the server, for example from its dashboard before you leave: `/handover to laptop` in the
server session leaves the session there for the laptop for 2 hours, and `/handover take` on the laptop shows it.

## 4. Without a connection: use a handover file

When the two machines cannot reach each other, move the session as a single file.

1. In the session on the first machine, run:

   ```
   /handover export
   ```

   This writes one handover file, `<title>-<date>-<time>.ctxh.tar`, into `~/hermes-handover` (setting
   `workspace_root`); the final message names it. The session is locked here, as in step 2. With
   `/handover export --keep` it stays active here instead, and the file is marked as a copy.
2. Copy the file to the other machine (USB stick, file share, …). It holds the conversation and the project files
   unencrypted, so keep it as safe as the project itself.
3. On the other machine, in any chat:

   ```
   /handover import ~/Downloads/my-session-20261110-180000.ctxh.tar
   ```

   Give the path from your home folder (`~/…`) or the full path. When the two machines are paired, the import
   checks that the file was signed by the paired machine. When they are not paired, add `--trust-unsigned`; the
   session is then marked as coming from an unverified file, and the plugin never starts a turn for it on its own.

## 5. Take a session back

Changed your mind, or the other machine is gone? In the locked session on this machine, run:

```
/handover reclaim
```

In a terminal: `hermes handover reclaim --session <session-id>`. The session is active here again. The copy on the
other machine stays active too: you now have two independent copies, and they are never merged. The answer says
whether the other machine was told.

## What moves and what stays here

Moves to the other machine:

- The whole conversation, with every tool call and tool output, and the session's side files (long tool outputs,
  hook outputs, terminal outputs). Attached images show again only where the same file path exists on the other
  machine.
- The project folder. In a git project: all branches, the stash, and changes you have not committed yet, new files
  included. Files that git ignores (build outputs, caches) stay here. Without git: the session's working folder, as
  it is.
- Large files stored with Git LFS, when git-lfs is installed on the other machine. Otherwise they arrive as small
  placeholder files, and the final message says so.
- Files outside the project that the conversation used — the plugin asks you first (setting `extra_paths`).
- A note for the agent on the other machine: what changed, what did not move, and where the files are now.

Stays here:

- Running programs, subagents and scheduled (cron) jobs. The note names them, with the command to pause a cron job
  that still belongs to the old session.
- Secret files: `.env` files, keys and credential folders such as `~/.ssh` or `~/.aws` — unless you name one with
  `--include-secret <path>`. Hermes' own credentials never move.
- Other git repositories inside the project (submodules, other checkouts). The plugin asks you first; a submodule
  travels only as the commit your project records.
- The session itself, locked (see step 2). `/handover reclaim` unlocks it.

Nothing on the other machine is overwritten: when a project folder there has changes of its own, the copy goes into
a fresh folder, and the final message names it.

## Safety in plain words

- **What a paired device may do.** With the *pull* right (to take) it can take any session of this Hermes profile,
  with its files. With the *push* right (to send) it can create sessions here and write project copies into the
  allowed folders (`allow_roots`, by default `~/hermes-handover`) — never into Hermes' configuration, nor into
  places that hold credentials, shell or autostart settings, or git and ssh configuration. It can start a turn here only
  when you switch on `allow_remote_continue`, and only while approvals are on.
- **What travels as it is.** The conversation, tool outputs, attached images and files go to the other machine
  unredacted: nothing is blacked out. Whatever the agent saw, the other machine gets. Committed git history travels
  with a project, including secret files that were committed. A handover file is not encrypted.
- **What stays here.** `.env` files, keys and credential folders stay here unless you name each one with
  `--include-secret <path>`; a general *yes* never sends them. Hermes' own credentials never leave.
- **The stored sign-in opens the whole dashboard.** The dashboard token (or user name and password) that the laptop
  stores for the server opens the server's whole dashboard — its settings, its secrets and its terminal — not only
  this plugin. Whoever gets the laptop's `.env` file gets that access. A reverse proxy in front of the server that
  passes only `/api/plugins/ctx_handover/` limits it. If you lose the laptop, run `/handover peers revoke laptop` on
  the server and change the server's dashboard token.
- **A pairing line is a password for 10 minutes** (by default). Keep it to yourself until it is used or has
  expired.
- **Network.** The plugin talks only to the machines you pair, and only while you pair, check (`doctor`), look at
  the status or hand over. Each handover tells the other machine facts about this one (system, host and user name,
  folders, versions, free disk space — never the values of environment variables). No other services, no telemetry,
  no update checks.

The exact list of what the plugin reads, writes, runs and sends is under [Disclosure](#disclosure).

## Reference

In short, how it works: `ctx_handover` moves a live session and the working files it needs to another Hermes
gateway you own — laptop ↔ server by pull, push or an offline bundle — and can continue it there: the transcript is
transferred row by row and verified by hashes over every message field, git repositories keep branches, stash and
uncommitted work, and the continuing agent gets a note about what changed.

### Words used below

- **gateway** — the Hermes service that runs your chats on a machine.
- **routes** — the web addresses under `/api/plugins/ctx_handover/` that the other machine calls.
- **source / target** — the machine you hand over from / to.
- **peer** — a paired device: another machine you paired with this one.
- **dialer** — the machine that opens the connection (usually the laptop); the other one only answers.
- **pull / push** — the dialer takes a session from the other machine / sends one to it.
- **transcript** — the conversation, with tool calls and tool outputs.
- **workspace** — the project folder that travels with the session.
- **bundle** — the handover file (`.ctxh.tar`) that `/handover export` writes and `/handover import` reads.
- **offer** — a session left on one machine for the dialer to take (`/handover to <device>` on the machine that
  does not dial).
- **gate** — the sign-in in front of the other gateway's dashboard: a dashboard token, or a user name and password.
- **job** — one handover; messages name it by a six-character job id.
- **commit point** — the point where a handover can no longer be undone.
- **in doubt** — it is unclear whether the other machine finished its part.
- **`needs_attention`** — the state `/handover status` shows when a handover stopped and needs you.
- **ledger** — the plugin's own small database in its data folder; it records pairings and handovers.
- **turn veto** — the lock on a handed-over session: while the plugin is loaded, it refuses new messages there.
- **compaction** — Hermes shortens a long conversation by summarising its older part.
- **LFS pointers** — the small placeholder files that stand for large files stored with Git LFS.
- **secrets list / denylist** — the file names and places the plugin treats as secret (secrets list) or never
  writes into (protected places).

### Limits

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
- After a pull, the target learns of a reclaim on the source only while the job runs; after it finished,
  `/handover status` on the target does not show it. A `reclaim` after a finished handover makes two independent
  copies: a reachable target of a push is told; after a pull, or when the target cannot be reached, the copy there
  stays active and is not told — the `reclaim` output says so.
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
- Under `plugins.isolation: host` Hermes runs this plugin in its plugin host, and the ctx_handover routes are
  reachable only through `hermes serve`: `hermes serve` forwards the ctx_handover routes there (each request and
  answer buffered; in the Hermes source this was built against, a forwarded request gets 504 after 60 s), and the
  gateway's own listener does not serve them — other devices use the `hermes serve` address. Run end to end under
  `host`: a paired device pulling a session from such a gateway through its `hermes serve` address (four of five lab
  runs; the fifth, right after `hermes serve` had restarted, was refused as `listener_missing`, cause not
  established), and `take` started on such a gateway. **Not verified** under `host`: pushes and offers to such a
  gateway, `--continue` there, and `to`, `export` or `import` started there — `in_process` (the default isolation)
  is the verified setup for these. `/handover doctor` lists the same. A gateway under `host` also has no
  `hermes handover` CLI (use the slash form) and no progress notice (`/handover status` shows progress), and Hermes
  buffers request bodies before this plugin's size limit applies.
- **Continuation and approvals.** A continuation (`--continue`, `allow_remote_continue`) never starts while
  approvals are off — `approvals.mode: off`, an approving `approvals.single_query_mode`, or a gateway running with
  `--yolo`/`HERMES_YOLO_MODE` — as far as the process that starts it can tell. The turn runs in the gateway. Inside
  it (`/handover` in its chats, routes on its own listener, its plugin host) the plugin sees all of that. A separate
  `hermes serve` or `hermes dashboard` serving the routes, and `hermes handover take|import --continue` in a
  terminal, read the config and identify the gateway through Hermes' own records; where they can identify the
  gateway they see the gateway's command line (its `--yolo`) but not its environment: a gateway given
  `HERMES_YOLO_MODE` only in its environment (for instance in a service unit) is not seen there, and the
  continuation is not refused — it starts, and the process that starts it logs a warning that approvals were not
  checked; leave `allow_remote_continue` off on such a gateway. Where they cannot identify the gateway — it runs in
  another container or PID namespace (Hermes' own `docker-compose.yml` runs gateway and dashboard as separate
  containers), or Hermes' gateway status cannot be read — no continuation starts (a `--yolo` in that gateway's own
  runtime record still counts as approvals off): the final message says why and names the command that starts the
  turn there by hand, `hermes chat --resume <id>` (or open the session there and send a message). `/handover
  doctor` says per process what it could check ("not checked" where it could not, and whether a continuation
  started there is refused).
- **Secret names are matched broadly, on purpose.** The secrets list counts wherever a name lies, also inside a
  project: a folder named like a secret file (`credentials`, for instance a Go package of that name) and every
  credential folder name (`.ssh`, `.aws`, `.gnupg`, `.kube`, `.docker`, `.azure` and the others of
  `policy.SOURCE_DENY_DIRS`) with everything below it. In a workspace without git such a folder stays here as a
  whole, and you learn it from the final message ("Secrets left out") and the note, not from the summary before the
  handover. In a git workspace a folder named like a secret file travels like any other, and the tracked files below
  a credential folder name are listed before the handover ("Tracked files on the secrets list", see **Unredacted**).
  To send such a folder anyway, name it with `--include-secret <path>` — below a credential folder name each file as
  well.
- **A pairing code is all a pairing needs.** Redeeming a code does not prove that the device holds the private key of
  the public key it registers: whoever has a valid, unused pairing code can pair a public key that is not theirs
  (public keys are no secret — every bundle names its exporter's). That pairing gets no access, because every later
  request must be signed with the private key of that key; but it replaces the earlier pairing of that key here: that
  device is unpaired here (it pairs again) and its open handovers here are aborted. Treat a pairing code like a
  password until it is used or expires (10 minutes by default).
- **Sessions from older Hermes builds.** Messages written before Hermes stamped message and tool-call identities
  get stable identities derived from the original rows during the transfer; the source session is not modified.
  If Hermes backfills those identities on the source while a handover is running, the target refuses the late
  changes (`verify_failed`) and the handover starts over from a fresh export when you start it again: the failed run
  ends before anything is committed and leaves the source session usable and unlocked — nothing is accepted
  silently.
- **Messages typed into a handed-over session.** While ctx_handover is loaded, every message sent in the
  handed-over session is withheld (no model call) and answered with a notice where the session continues.
  `/handover reclaim` removes these attempts from the end of the conversation and lists their text. Attempts that
  a context compaction moved into an earlier part of the session stay in its history, because Hermes allows no
  changes there; reclaim still lists their text and says how many remain.
- **Display after an undone message.** If a message that a compaction carried forward was later undone on the
  source, the target's chat display shows the earlier copy of that message again. What the model sees is
  identical on both sides; undone messages themselves are not transferred.
- **Pulling from a machine that goes quiet.** When a pull stops talking to the source for
  `participant_idle_timeout_min` (15 minutes by default), the source releases its locks and the session there is
  usable again; `/handover resume` locks it again and continues.
- **`/handover resume` right after Hermes starts.** When a Hermes process starts, the plugin's background clean-up
  looks at unfinished handovers. A `/handover resume` typed in that moment can lose against it: the handover then
  ends, the session here stays unlocked and usable, and you start the handover again.
- **A git command that died mid-way.** A git process killed in the middle of a command in a destination
  repository (power loss, killed process) can leave `.git/index.lock` behind. The plugin never removes it (it may
  belong to another git process): the step that resumes the handover refuses that repository and the handover
  ends marked `needs_attention`. Remove the lock once no git process runs there, then hand over again.
- **A stash made while a handover writes.** When the first writing step into an existing destination repository
  fails, the plugin rolls that repository back and drops every stash entry beyond the number it found when it
  prepared the step. A `git stash` you make there in that window is dropped with them; it stays recoverable until
  git's garbage collection (`git gc`) prunes it — `git fsck --unreachable` lists it, as git's stash manual
  describes.
- **The size check needs the context window there.** Whether the session fits the model on the other side is
  checked against that model's context window, and Hermes gives a plugin only a `context_length` set in that
  gateway's `config.yaml` (`model.context_length`, or `context_length` of the provider entry or of its
  `models.<model>`). Without it nothing is asked about the size — the summary says that the context window there is
  unknown — and the other side compacts an oversized session on its first turn as usual.
- **No relay through ctx.** A handover goes directly between the two gateways or through a bundle file you copy;
  ctx (optional) only keeps a record of it. Relaying a handover through ctx blobs is not built.

### What the lock on the source guarantees

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

### Set up in detail

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
   (`CTX_HANDOVER_PEER_TOKEN`, `CTX_HANDOVER_PEER_PASSWORD`), never in `config.yaml`. Pairing copies them into
   variables of that gateway alone (`CTX_HANDOVER_PEER_TOKEN_<peer id>`, see Disclosure), so the form then takes the
   next gateway's credential; to change a paired gateway's credential later, edit its variable (Keys page of the
   dashboard or `.env`; `/handover doctor` names it) or pair again.
4. **Pair:** in a chat on the server run `/handover pair --name laptop`. Copy the single line that starts with
   `ctxh-pair:` and run `/handover pair ctxh-pair:…` in a chat on the laptop. The line carries the address, a
   one-time code (10 minutes, single use) and the server's key fingerprint, which the laptop checks itself — it
   never carries a dashboard credential. For a plain-HTTP address that only your VPN reaches (e.g. Tailscale) add
   `--allow-insecure`. The laptop names the server as the line does, or takes the next free name (`server-2`) when
   that one is taken there; with `--name` you choose it, and a taken name is refused before the code is used.
5. `/handover doctor` on both sides checks the setup and says what to fix. `/handover doctor --measure` also finds
   the request-body limit of a proxy in front of each paired gateway (test uploads, see Disclosure).

### Use in detail

| Situation | Where | Command |
|---|---|---|
| Take the server session to the laptop before a flight | laptop chat | `/handover take` (picks the only paired gateway; shows offers and the 20 most recent sessions to choose from) |
| …or start it from the server session | server chat | `/handover to laptop` — leaves an offer (2 h); then `/handover take` on the laptop |
| Back to the server after landing | the laptop session | `/handover to server` — repositories return into their original folders when nothing changed there meanwhile; otherwise (also when a file git ignores there is in the way) they land in a fresh folder — `<folder>-handover-<time>` beside it when that lies under `allow_roots`, else `<workspace_root>/<name>-handover-<time>` — which the final message names |
| Before closing time, let the agent keep working | the session | `/handover to server --continue "run the tests and fix what fails"` (needs `allow_remote_continue` on the server) |
| A turn is still running | the session | add `--wait` to hand over as soon as it finishes |
| No connection | source / target | `/handover export` writes one `.ctxh.tar`; `/handover import <file>` on the other machine (`export --keep` leaves this session active and marks the bundle as a copy; status and the final message say so) |
| Take it back here | the source session | `/handover reclaim` (`--force` while a handover is being committed); in a terminal `hermes handover reclaim --session <id>` (or `reclaim <id>`) |

The session's workspace is the git repository around its working folder, unless git ignores the working folder
there (a scratch or build folder inside a checkout) or carries nothing from it but placeholder files such as `.gitkeep`
while other files lie there (`tmp/*` with `!tmp/.gitkeep`): then only the working folder travels, as a file tree, and
the repository around it stays here. Files git ignores do not travel with a repository (build outputs, caches), also
when the conversation names them. A file the conversation names in a folder git ignores in another repository is an
item of its own outside the project, never the repository around it; a file git ignores only by its name, in a folder
it does not ignore (`*.log` in `src/`), still points the plan to that repository, which does not carry the file.
Git LFS files travel as their objects and are checked out on the other side with `git lfs checkout`, never from a
network; without git-lfs there they stay pointers and the final message says so. The size limit (`max_total_gb`, per
handover) and the free-space check at the destination count that content before anything is written: each shipped
object once and, when git-lfs is installed there, once more for every file that points to it. When a path that
`git lfs checkout` would write there is a symbolic link or lies behind one (git-lfs before 3.7.1 writes through
such links, CVE-2025-26625), the plugin does not run it: the files stay pointers and the final message names the
link.

Before anything is locked the plugin shows what would move: the workspace and its size, every item outside the project
with its size (the 10 largest, then how many more), the repositories inside the workspace that stay here (each
initialised submodule and each clone git does not ignore, also one the session never touched), the paths you named
with `--include` that this machine's policy leaves out (with the reason, also with `--yes`), and whether
the context window of the model there is unknown. It asks for a confirmation when items are only proposed, a destination
has local changes (a fresh folder is used), the handover is larger than `confirm_above_mb`, a secret is included with
`--include-secret`, the model there does not work offline (this session's model did), items outside the project are
included, instruction files arrive from an unverified source, the session's working folder is left out by the policy
(for example because it lies inside the Hermes home; the summary names the folder, and the project does not come along),
the workspace is the whole repository around a working folder that holds none of its tracked files — typically somebody
else's checkout (the summary names the workspace and its size next to the working folder's; a tracked subfolder such as
`repo/src` asks nothing, the summary still names the repository and its size), a repository inside the workspace (a
submodule or another clone) stays here with its files and changes (a submodule travels only as the commit the project
records), or the session is larger than the context window of the model there (the summary and the note name both sizes;
the other side compacts the conversation on its first turn). The choices are *yes*, *yes without the proposed items*,
*show the plan and stop*, *cancel*; `--yes` skips the question. `--yes` does not confirm a left-out working folder: a
person answers that question (without an answer nothing is locked), or `--no-workspace` hands the session over without
its folder on purpose. Nor does it confirm a session larger than the context window there: a person answers, or
`/compress` in the session first makes it fit. `--yes` does not confirm such a whole repository either, nor a repository
inside the workspace that stays here: a person answers (at the export; `import` does not ask it again, the bundle holds
what was confirmed). Files that look like secrets are never sent by a blanket yes (neither `--yes` nor the *yes* choice
takes them along); name each one with `--include-secret <path>`. An unanswered question (no reply within Hermes'
question timeout `agent.clarify_timeout`, 90 s by default) continues without the proposed items when those are the only
reason and cancels otherwise — nothing is locked in either case.

The final message names the session on the other side (title and session id), its folder, the model and whether it
works offline, and what to do next. A received session is stored, not open: it is listed in the Desktop sidebar and in
`hermes sessions list`, and `hermes --resume <session-id>` opens it (a gateway's list of open sessions, such as the
TUI switcher, shows it only after it was opened once). After a pull the final message also lists paths you asked for
with `--include` that the other side does not release (withheld by source policy — a pull's own hints only narrow what
the source offers); after a handover from here it names the `--include` paths this machine left out. On the receiving
side `/handover status <job>` shows the note the agent gets with your first
message there; the note is not shown in the transcript view. Hook-output files arrive in the received session's own
folder (`hook_outputs/<session id>/` in the Hermes home there, same file names); the conversation still names the old
folders, so the note lists, for each old folder, the folder its files are in now. `/handover status` also shows
the jobs another gateway runs with this one — a push that arrived here, a pull that took a session from here —
listed with this gateway's role (`target, from <peer>` or `source, to <peer>`) and, for an arrival, the session it
became here.

All commands: `/handover to | take | export | import | status | peers | pair | reclaim | cancel | resume | setup |
doctor | identity | gc | logs | help` — `/handover help <command>` shows the options. `hermes handover <same>` runs
them in a terminal (`--json` for scripts, `--session <id>` names the session; exit codes 0 ok, 1 failed, 2 usage, 3
busy/conflict, 4 auth/gate, 5 in-doubt/suspended; a refusal carries `ok: false` and `error.code` in `--json` — a
`reclaim` refused while a handover is being committed exits 3, and so does a `cancel` that cancels nothing because
the handover is done, aborted or past its commit point; the answer names which). `status`, `cancel`, `resume`, `logs`
and `gc --job` take the six-character job id the messages print (or any other unique start or end of the id). A
handover stuck cancelling because the other gateway does not confirm (unreachable, or it no longer knows the job)
ends here with `/handover cancel <job> --force`: this machine's side is released or discarded the normal way, the
answer says what may remain on the other gateway, and a handover past its commit point is refused (exit 3). A
handover with a gateway you removed here with `/handover peers remove` cannot be ended with `--force`: the answer
says that it could not be ended here.
Administrative and data-moving commands (`pair`, `setup`, `peers`, `identity`, `take`, `to`, `export`, `import`,
`reclaim`, `resume`, `gc`, `cancel`) answer only in the local CLI, TUI or Desktop; on messaging platforms they are
refused.

The agent has a `handover` tool: it can show the plan, name resources the file scan cannot see (paths, tools and
the names — never values — of environment variables; anything outside the project is only proposed) and ask for a
handover. It starts one itself only with `allow_agent_start`, then without proposed items and stopping at any
other confirmation.

### Settings

Settings → Plugins → ctx_handover (or `plugins.entries.ctx_handover.settings` in `config.yaml`). Every key has a
working default; the most relevant:

| Key | Default | Meaning |
|---|---|---|
| `inbound` | `auto` | accept handovers only while a device is paired or a pairing code is pending; `on`, `off` |
| `listen_url` | `""` | address put into pairing strings (else `webapp.public_url` / `dashboard.public_url`) |
| `peer_gate`, `peer_token`, `peer_username`, `peer_password`, `peer_extra_headers` | `session_token` | how this device signs in to the other gateway's dashboard |
| `workspace_root`, `allow_roots` | `~/hermes-handover` | where received workspaces and bundles go. A destination lies under `allow_roots`, except a repository returning to the folder it came from (return trip: this machine recorded that folder when it handed the repository away), a `--dest`/`--map` typed here, and a folder the same peer delivered into here before (a later handover from that peer may write there again); the denylist under **Rights** applies to every destination. When the home folder lies inside the Hermes home (the official Docker image), the default is `<HERMES_HOME>/handover-workspaces`, the one folder of the Hermes home a handover may write to; `/handover setup` shows the folder in use |
| `export_roots` | `[]` | extra paths a device with the pull right may request beyond this machine's own plan |
| `max_total_gb`, `max_file_mb`, `max_session_mb`, `confirm_above_mb` | 10, 100, 512, 500 | size limits and the confirmation threshold |
| `refine` | `auto` | ask a model once per handover which resources the work needs (`side_question`, `llm`, `off`) |
| `extra_paths` | `ask` | files outside the project that the conversation used (`auto`, `off`) |
| `model_mapping` | `auto` | keep the session's model when the receiving machine can serve it, else its default |
| `allow_remote_continue` | `false` | let a paired device start one continuation turn here |
| `allow_agent_start` | `false` | let the agent start a handover it was asked for |
| `ctx_persist`, `ctx_category` | `auto`, `handover` | ctx records (only with ctx_checkpoint and your consent) |

The full list with all limits and timeouts is the `config_schema` in `plugin.yaml`. A value of the wrong type or
outside its choices counts as that key's default — for `inbound` as `off`, so an unreadable value never opens it (a
bare YAML `off` means `off`); `/handover doctor` names each such value.

### Disclosure

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
disabled for received data; git, and what git starts (git-lfs, helpers), gets an environment built from an
allowlist — the search path, the home folder, `XDG_CONFIG_HOME`, the temp folder, the time zone and on Windows the
system variables git needs to start — and no other variable of the Hermes process, so provider keys and tokens never
reach it (no git call talks to a remote, so no SSH agent socket, proxy or CA setting either); `<tool> --version` for
a fixed list of 30 developer tools plus those you name in the `tool_probe` setting, each run with only the search
path (and the Windows system variables) of this process and a home and temp folder of its own in a scratch folder in
the system temp directory (`ctxh-probe-*`, removed afterwards); with `--continue`,
one detached `hermes chat --resume <id>` single-query run on the target (Hermes' single-query approval policy
applies, default deny), started with this gateway's environment — the turn needs the model provider's keys — minus
the auto-approve variables, and never when
approvals are off there as far as the starting process can tell (see Limits), nor when that process cannot identify
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
`store` (to link the record to it); a side finished without ctx (e.g. offline) is recorded later. The link works for
a root session id that ctx_checkpoint uses unchanged in its titles — Hermes' timestamp ids and UUIDs; a longer id or
one with other characters (such as Hermes' 70-character `local-<sha256>` ids), which ctx_checkpoint shortens and
marks with a hash, is not found that way, and its record is stored without the link. ctx is never
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
refused by `import` unless you add `import --trust-unsigned`; such a session arrives marked as from an unverified
bundle, never starts a continuation and binds no return trip to its exporter. Its instruction files ask for a
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

### Troubleshooting

- **`hermes handover` or `/handover` is unknown on a Hermes installed from a source checkout.** Such an
  installation reads its own version with `git tag --merged HEAD` under a 3-second limit; on a busy machine the
  limit runs out, Hermes falls back to an older base version, and this plugin (it needs 0.22.6 or newer) is skipped
  in that process — the log says `Plugin 'ctx_handover' skipped: requires hermes >=0.22.6`, and
  `hermes plugins enable ctx_handover` can be refused the same way. Run the command again, or restart the gateway,
  once the machine is less busy. Packaged installations (Docker, Nix) carry a version stamp and are not affected.

### Tests

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

### Layout

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
