<p align="center"><img src="../../assets/ctx_handover.webp" alt="ctx handover: close the lid, the session goes on" width="100%"></p>

# ctx_handover

**Close the lid. The session goes on.**

`ctx_handover` moves a running [Hermes](https://github.com/NousResearch/hermes-agent) session, with the conversation
and the project folder it works in, from one of your machines to another, and back. Send it to your server before you
close the laptop. Pick it up again on the road. Or give the agent one more task that it finishes on the server while
the laptop is in your bag.

```
/handover to server                                        after work: laptop → server
/handover to server --continue "run the tests, fix them"   …and the agent keeps working there
/handover take                                             on the road: server → laptop
/handover export                                           no connection: one file you carry over
```

[![Watch the 90-second explainer: Close the lid](../../assets/ctx_handover_video.jpg)](https://gottz.de/close-the-lid.mp4)

## What you get

- **The whole session.** Every message, tool call and tool output arrives on the other machine, not a summary.
- **Your project as it is.** A git project keeps its branches, its stash and the changes you have not committed yet,
  new files included.
- **Nothing is overwritten.** If the folder on the other machine has changes of its own, your copy lands in a fresh
  folder next to it.
- **One copy goes on.** The session is locked on the machine you left, so you never continue the wrong one by accident.
- **The agent knows what happened.** It gets a note: what changed, what did not move, and where the files are now.
- **Only your machines.** No cloud service in between, no telemetry. Works without ctx.

## How it works

Two machines that run Hermes and that you own, called *your laptop* and *your server* here. You pair them once. After
that a handover is one command, and it runs in six steps:

| Step | What happens |
|---|---|
| 1. Plan | The plugin shows what it would move: the project folder and its size, other files the conversation used, and what it leaves out. You say yes. |
| 2. Pause | The session is paused on this machine, so nothing changes while it travels. |
| 3. Copy and check | The conversation is copied message by message, and both machines compare hashes of every message. A git project travels as git data, other folders as files. |
| 4. Land | The other machine stores the session and puts the project into a folder. A project that came from there goes back into its old folder when nothing changed there meanwhile. |
| 5. Switch | Only when the other machine confirms that everything arrived is the session ended and archived here. If anything fails before that, the session here stays usable as before. |
| 6. Carry on | You open the session on the other machine. With `--continue` the agent there starts on your task by itself. |

There are three ways to move a session:

| Way | Command | Use it when |
|---|---|---|
| Send | `/handover to <device>` | you sit at the machine that has the session |
| Take | `/handover take` | you sit at the machine that should get it |
| File | `/handover export`, then `/handover import <file>` | the two machines cannot reach each other |

The laptop always opens the connection, so it works from behind a home router or a hotel Wi-Fi. The server only needs
an address the laptop can reach: its Hermes dashboard, public or inside a private network such as Tailscale.

## Install

On both machines:

```bash
hermes plugins install ctx_handover
hermes plugins enable ctx_handover
```

In Hermes Desktop: Capabilities → Plugins, once for the server's gateway and once for *This device*.

You need Hermes 0.22.6 or newer on both machines, and `git` 2.38 or newer if your project is a git repository. Tested
on Linux; macOS and Windows have had no end-to-end runs yet.

## Set up once: pair the two machines

1. **Server:** tell the plugin the address your laptop uses to reach the server's Hermes dashboard, then restart Hermes
   there.

   ```bash
   hermes config set plugins.entries.ctx_handover.settings.listen_url https://server.example.net
   hermes gateway restart
   ```

2. **Server, in a chat:** `/handover setup` checks the address and tells you which sign-in the laptop needs: a
   dashboard token, or a user name and password.
3. **Laptop:** enter that sign-in under Settings → Plugins → ctx_handover.
4. **Server, in a chat:** `/handover pair --name laptop` prints one line that starts with `/handover pair ctxh-pair:`.
   Paste that line into a chat on the laptop. It is valid for 10 minutes and works once.
5. **Both:** `/handover doctor` checks the setup and says what to fix.

The full walk-through, with every option, is in [Set up](docs/setup.md).

## Everyday use

| You want to | Where | Command |
|---|---|---|
| Send the session to the server after work | the laptop session | `/handover to server` |
| …and let the agent keep working there | the laptop session | `/handover to server --continue "run the tests and fix what fails"` |
| Hand over as soon as the running answer is done | the session | add `--wait` |
| Bring a session to the laptop | any laptop chat | `/handover take` |
| Move it as a file, without a connection | first machine, then the other | `/handover export`, then `/handover import <file>` |
| Take a session back | the locked session | `/handover reclaim` |
| See what is going on | any chat | `/handover status` |

Wait for the final message before you close the laptop: the laptop does the copying. The final message names the
session on the other machine and its folder.

`--continue` needs one switch on the server, `allow_remote_continue`. That turn runs with nobody there to approve
anything, so every command that would ask for your approval is refused, and it never starts on a server where
approvals are switched off.

All commands and options: `/handover help`, and [Use](docs/usage.md).

## What moves and what stays

| Moves to the other machine | Stays here |
|---|---|
| The whole conversation, with tool calls and tool outputs | Running programs, subagents and scheduled (cron) jobs; the note names them |
| The project folder; in a git project all branches, the stash and uncommitted changes | Files that git ignores, such as build outputs and caches |
| Large files stored with Git LFS, when git-lfs is installed there | Secret files: `.env` files, keys, folders such as `~/.ssh`, unless you name one with `--include-secret <path>` |
| Files outside the project that the conversation used; the plugin asks first | Hermes' own credentials, always |
| A note for the agent on the other machine | The session itself, locked; `/handover reclaim` unlocks it |

## Safety in short

- **Nothing is blacked out.** The conversation, tool outputs and project files travel as they are. Whatever the agent
  saw, the other machine gets. A handover file is not encrypted.
- **Secret files stay** unless you name each one. A general *yes* never sends them.
- **A paired device can take sessions of this Hermes profile**, or write project copies into the folders you allow
  (`~/hermes-handover` by default), depending on the right you gave it. It never writes into Hermes' configuration or
  into places that hold credentials, shell or autostart settings.
- **The stored sign-in opens the server's whole dashboard**, not only this plugin. A reverse proxy that passes only
  `/api/plugins/ctx_handover/` limits that. If you lose the laptop, run `/handover peers revoke laptop` on the server
  and change the dashboard token.
- **Your AI model is asked once per handover** which files the work needs. Setting `refine` to `off` stops it.

### Disclosure

- **Network:** HTTPS only to the machines you pair, and only while you pair, check, look at the status or hand over.
  Plain HTTP only on loopback or for a device you mark `--allow-insecure`. No third-party services, no telemetry, no
  update checks.
- **Sent to the other machine:** the session and its files, unredacted, and facts about this machine (system, host and
  user name, folders, versions, free disk space), never the values of environment variables.
- **Inbound:** once a device is paired, signed routes under `/api/plugins/ctx_handover/v1/`, behind the dashboard
  sign-in.
- **Stored credentials:** the other machine's dashboard sign-in and the plugin's own identity key, in the profile's
  `.env` through Hermes' own reader and writer.
- **Commands:** `git`, without a shell and without your provider keys in its environment; version checks of developer
  tools; with `--continue`, one `hermes chat --resume` turn on the other machine.
- **Background work:** threads per handover, and a cache in the plugin's data folder (up to 5 GiB for 3 days by
  default).

The complete list of what the plugin reads, writes, runs and sends is in [Safety and disclosure](docs/security.md).

## Limits

- Gateways that Hermes Desktop reaches only through an SSH tunnel, a Nous/OAuth sign-in or Hermes Cloud have no
  sign-in the plugin can use. For them only the handover file works.
- Running programs, subagents and cron jobs do not move.
- Two copies that both went on are never merged.
- There is no Desktop page of its own yet. Everything runs through `/handover …` in a chat or `hermes handover …` in a
  terminal.
- This is not the core `/handoff`, which sends a session to a messaging app. It is not the hermes-handoff or
  session-relay plugins either.

Every limit, with the details: [Limits](docs/limits.md).

## More

| Document | What is in it |
|---|---|
| [Set up](docs/setup.md) | What you need, pairing step by step, sign-in modes, proxies |
| [Use](docs/usage.md) | Every way to hand over, the confirmation, where projects land, all commands and exit codes |
| [Safety and disclosure](docs/security.md) | Rights of a paired device, what the lock guarantees, the full disclosure |
| [Limits](docs/limits.md) | What does not work, or only partly |
| [Reference](docs/reference.md) | Words used, settings, troubleshooting |
| [Development](docs/development.md) | Tests and code layout |

## License

MPL-2.0, part of [ctx](https://github.com/GottZ/ctx). ctx is a separate knowledge store; the plugin works without it.
With `ctx_checkpoint` as memory provider and your consent in `/handover setup`, each handover is also recorded in ctx.
