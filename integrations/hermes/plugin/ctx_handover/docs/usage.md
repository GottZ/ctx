# Use ctx_handover

[← ctx_handover](../README.md)

The four ways to move a session, how to take one back, and what travels. The machines must be paired first ([Set up](setup.md)), except for a handover file.

## 1. After work: send the session to your server

In the laptop session you want to move, run:

```
/handover to server
```

Before anything happens, the plugin shows what it would move: the project folder and its size, other files the
conversation used, and what it leaves out. When it asks, answer *yes* — and answer while you are still there: without an
answer within Hermes' question time limit (90 seconds by default), the handover goes on without the files it only
proposed, or stops if there was another reason to ask. Then it copies the conversation and the files to the server.
**Wait for the final message before you close the laptop** — the laptop does the copying. The final message names the
session on the server and its folder.

If the agent is still working on an answer, add `--wait`: the handover starts as soon as the answer is done.

Afterwards the session is locked on the laptop: Hermes marks it as ended, archives and hides it, and adds "(handed over
to server …)" to its title, so it cannot go on here by accident. On the server, the session is listed in the Desktop
sidebar and in `hermes sessions list`; `hermes --resume <session-id>` opens it.

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

The server then runs one turn with that task: the agent works on it until it has answered, then the session waits for
you. Nobody is there to approve anything in that turn, so every command that would ask for your approval is refused —
Hermes' rule for such unattended turns (`approvals.single_query_mode`, `deny` by default). The turn never starts when
approvals are switched off on the server (for example a gateway started with `--yolo`), nor when the plugin cannot find
out which Hermes service on the server would run it — for example when the dashboard that receives the handover runs in
another container than the gateway, as Hermes' own Docker setup does. Then the final message names a command that starts
the turn on the server by hand, for when you are back at a chat there. `--continue` without a task sends "Continue where
we left off."

After you switch `allow_remote_continue` on, run `/handover doctor` on the server: its `allow_remote_continue` line says
whether approvals could be checked. One case the plugin cannot see: if the server's gateway gets `HERMES_YOLO_MODE` from
its environment (for example from its service settings), leave `allow_remote_continue` off there. The details are under
[Limits](limits.md) (*Continuation and approvals*).

## 2. On the road: bring the session to your laptop

On the road, the laptop still needs to reach the server's address — if only your private network (such as Tailscale)
reaches it, switch that on first. Then, on the laptop, in any chat:

```
/handover take
```

It lists the sessions on the server — sessions left for you first, then the 20 most recent ones — and asks which one to
take. To take the most recently used session on the server without a question:

```
/handover take server --latest
```

The session and its project folder are copied to the laptop. The final message names the session and its folder; open it
from the Desktop sidebar or with `hermes --resume <session-id>`. On the server, the session is now locked, as described
in section 1.

A project that came from this laptop goes back into its old folder when nothing changed there meanwhile. Otherwise it
lands in a fresh folder, and the final message names it; the session works in the fresh folder, and your old folder
stays as it is. Other projects arrive under `~/hermes-handover` — the folder `hermes-handover` in your home folder
(setting `workspace_root`; with Hermes' official Docker image it is another folder, and `/handover setup` shows the one
in use).

You can also start on the server, for example from its dashboard before you leave: `/handover to laptop` in the server
session leaves the session there for the laptop for 2 hours, and `/handover take` on the laptop shows it.

## 3. Without a connection: use a handover file

When the two machines cannot reach each other, move the session as a single file.

1. In the session on the first machine, run:

   ```
   /handover export
   ```

   This writes one handover file, `<title>-<date>-<time>.ctxh.tar`, into `~/hermes-handover` (setting `workspace_root`);
   the final message names it. The session is locked here, as in section 1. With `/handover export --keep` it stays
   active here instead, and the file is marked as a copy.
2. Copy the file to the other machine (USB stick, file share, …). It holds the conversation and the project files
   unencrypted, so keep it as safe as the project itself.
3. On the other machine, in any chat:

   ```
   /handover import ~/Downloads/my-session-20261110-180000.ctxh.tar
   ```

   Give the path from your home folder (`~/…`) or the full path. When the two machines are paired, the import checks
   that the file was signed by the paired machine. When they are not paired, add `--trust-unsigned` — only for a file
   you made and carried over yourself. The session is then marked as coming from an unverified file, and the plugin
   never starts a turn for it on its own.

## 4. Take a session back

Changed your mind, or the other machine is gone? Open the locked session on this machine — Hermes keeps it in its
archive (in Desktop, the archive view) — and run:

```
/handover reclaim
```

In a terminal: `hermes handover reclaim --session <session-id>` (the id of the session on this machine). The session is
active here again. The copy on the other machine stays active too: you now have two independent copies, and they are
never merged. The answer says whether the other machine was told.

## What moves and what stays here

Moves to the other machine:

- The whole conversation, with every tool call and tool output, and the session's side files (long tool outputs, hook
  outputs, terminal outputs). Attached images show again only where the same file path exists on the other machine.
- The project folder. In a git project: all branches, the stash, and changes you have not committed yet, new files
  included. Files that git ignores (build outputs, caches) stay here. Without git: the session's working folder, as it
  is.
- Large files stored with Git LFS, when git-lfs is installed on the other machine. Otherwise they arrive as small
  placeholder files, and the final message says so. The same happens when a link is in their way there and git-lfs is
  older than 3.7.1; the final message then names the link.
- Files outside the project that the conversation used — the plugin asks you first (setting `extra_paths`).
- A note for the agent on the other machine: what changed, what did not move, and where the files are now.

Stays here:

- Running programs, subagents and scheduled (cron) jobs. The note names them, with the command to pause a cron job that
  still belongs to the old session.
- Secret files: `.env` files, keys and credential folders such as `~/.ssh` or `~/.aws` — unless you name one with
  `--include-secret <path>`. Hermes' own credentials never move.
- Other git repositories inside the project (submodules, other checkouts). The plugin asks you first; a submodule
  travels only as the commit your project records.
- The session itself, locked (see section 1). `/handover reclaim` unlocks it.

Nothing on the other machine is overwritten: when a project folder there has changes of its own, the copy goes into a
fresh folder, and the final message names it.

## Use in detail

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
network (for that one call the plugin gives git the standard Git LFS filter, which every other git call on received
data runs without); without git-lfs there they stay pointers and the final message says so. The size limit
(`max_total_gb`, per handover) and the free-space check at the destination count that content before anything is
written: each shipped object once and, when git-lfs is installed there, once more for every file that points to it.
When a path that `git lfs checkout` would write there is a symbolic link or lies behind one, and git-lfs there is
older than 3.7.1 or its version cannot be read, the plugin does not run it (git-lfs before 3.7.1 writes through such
links, CVE-2025-26625): the files stay pointers, and the final message names the link and recommends git-lfs 3.7.1 or
newer. With git-lfs 3.7.1 or newer the checkout runs in that case too; git-lfs itself then leaves a file behind such a
link as a placeholder, and the plugin does not report it. While it fills the files, git-lfs can also copy files that
the received `.gitattributes` marks for Git LFS but that arrived as ordinary files into the repository's LFS store; the
size check above does not count those copies (at most the size of those files once more).

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
listed with this machine's role ("arriving here from <peer>" or "leaving here for <peer>") and, for an arrival,
the session it became here.

All commands: `/handover to | take | export | import | status | peers | pair | reclaim | cancel | resume | setup |
doctor | identity | gc | logs | help` — `/handover help <command>` shows the options. `hermes handover <same>` runs
them in a terminal (`--json` for scripts, `--session <id>` names the session; exit codes 0 ok, 1 failed, 2 usage, 3
busy/conflict, 4 auth/gate, 5 in-doubt/suspended; a refusal carries `ok: false` and `error.code` in `--json` — a
`reclaim` refused while a handover is being committed exits 3, and so does a `cancel` that cancels nothing because
the handover is done, aborted or past its commit point; the answer names which). `status`, `cancel`, `resume`, `logs`
and `gc --job` take the six-character job id the messages print (or any other unique start or end of the id). A
handover stuck cancelling because the other gateway does not confirm (it is unreachable, it removed or revoked this
machine on its side, or it no longer knows the job) ends here with `/handover cancel <job> --force`: this machine's
side is released or discarded the normal way, the answer says what may remain on the other gateway, and a handover
past its commit point is refused (exit 3). A handover with a gateway you removed or revoked here (`/handover peers
remove`, `/handover peers revoke`) cannot be ended with `--force`: the answer says that it could not be ended here.
Administrative and data-moving commands (`pair`, `setup`, `peers`, `identity`, `take`, `to`, `export`, `import`,
`reclaim`, `resume`, `gc`, `cancel`) answer only in the local CLI, TUI or Desktop; on messaging platforms they are
refused.

The agent has a `handover` tool: it can show the plan, name resources the file scan cannot see (paths, tools and
the names — never values — of environment variables; anything outside the project is only proposed) and ask for a
handover. It starts one itself only with `allow_agent_start`, then without proposed items and stopping at any
other confirmation.
