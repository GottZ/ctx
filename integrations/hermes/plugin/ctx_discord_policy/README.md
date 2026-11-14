# ctx_discord_policy

Per-server Discord policy for [Hermes](https://github.com/NousResearch/hermes-agent): the bot takes part in
server conversations while tools stay reserved for the operators configured on the host, and each server is
configured from inside Discord with the `/policy` command.

- **Operators only get tools.** Who is an operator comes from the Hermes configuration on the host, never from
  Discord: no role, server admin or server guidance text can grant tools. Members' turns get only the lanes the
  operator enabled for that server (for example reading the current channel's history).
- **Per-server settings in Discord.** Server managers set channels, the bot's name and wake words, reply
  language, progress display and a server guidance text; operators additionally set worklog channels, what
  members may let the bot do, the Hermes profile and the models a server uses. Everything is per bot and per
  server.
- **Adapts to Hermes updates.** Every place where the plugin hooks into Hermes is probed at startup. Safety
  checks fail closed (members get no access) and comfort features fall back to Hermes' own behaviour; the result
  is visible in `/policy` → Status and in `hermes ctx-discord-policy status`. A daily run tests the plugin against
  Hermes `main` and the latest releases.

**Status:** version 3.0.0 is in development and not released yet. Everything described below is in `main`.

## In a server

Once the policy is active in a server, the bot answers when it is addressed:

- by a mention or a reply to one of its messages;
- by one of the server's **wake names** spoken to it ("Nova, …", "hey Nova", "…, Nova?"); a name that is only
  mentioned ("ask Nova later") does not wake it. Without wake names it answers to its configured name, else to
  its display name in the server;
- by a **reaction** with the server's wake emoji on any message: the reactor gets an answer to that message, in
  their own lane (a member who reacts to an operator's message gets a member's turn);
- in **ambient channels** (and the threads beneath them) the bot reads along and answers when it has something
  to add. Channels marked "never respond in" stay silent for members.

Bots, webhooks and system messages never wake it. With a worklog channel set, operator turns that use tools
write their output into a worklog thread there and copy the final answer back to the origin; members are never
answered in the worklog channel or its threads.

**Links and channels in a message.** When a message the bot answers links Discord messages (up to three),
mentions channels (up to two: their latest messages) or points at earlier media (`^`, `👆` on its own), that
content is attached to the turn as quoted data. It is read with the rights of the member who wrote the message
and only from this server. Members' turns (addressed or ambient) read only channels that everyone who sees the
answer can read; operator turns read without that limit, so they are hydrated only when the operator addresses
the bot, never from ambient chatter. Unaddressed messages are not hydrated. A private thread, the worklog channel and its threads are read only by a turn that
runs inside them. Operators switch it per lane in Member tools and Operator turns.

## /policy pages

| Page | Who | What |
|---|---|---|
| Overview, Status | managers, operators | activate, pause, remove; activation requests; the protection status |
| Identity | managers, operators | bot name, wake names, greetings, reply language, wake emoji |
| Channels | managers, operators | ambient channels and "never respond in" channels |
| Guidance | managers, operators | the server's guidance text: edit, upload, preview, history, reset |
| Progress | managers, operators | how tool progress is shown (rotate or separate messages) |
| Models | operators; managers pick from a released list | the models of members' turns and of operator turns |
| Worklog | operators | worklog channel, archive time, copy of the final answer |
| Member tools | operators | channel history, message links, skills, knowledge entries for members' turns |
| Operator turns | operators | channel context, whether server guidance applies to operator turns |
| Managers | operators | who manages the server's policy, the audit channel |
| Profile | operators | the Hermes profile this server's turns run in |

Every change is written with its origin to the plugin's audit log; with an audit channel set, the server gets
one line per change (which settings, never their values). Operators get a direct message with a diff for every
change of a server's guidance, when the bot connects with a degraded safety check (at most once a day per
check), and when a chosen model can't be used (at most once a day per server and lane); `notify_operators`
switches all of them off.

**Profile per server.** The operator can run a server in another Hermes profile (its tools, memory and
credentials). The plugin writes a named entry into `profile_routes` of the default profile's `config.yaml`
(managed configurations are applied live at every start instead) and applies it to the running gateway.
Members' messages are admitted only while the running route table sends the server to the chosen profile, the
plugin is enabled there and the profile is not in `locked_profiles`; a switch drops members' messages that were
still waiting. Offered are profiles that Hermes serves with the plugin enabled and that run no Discord bot of
their own.

**Models per server.** Members' turns and operator turns can each use another model of the server's profile.
The model is used only where that profile has a credential for the provider and the session has no `/model` or
`channel_overrides` entry of its own; otherwise Hermes' choice stands, or, if the operator chose so, members'
messages are not answered until the model works again.

## Requirements

- Hermes 0.22.4 or newer with the Discord platform, plugins in the default in-process mode.
- A Discord application with the **Message Content** intent enabled (Hermes needs it as well).

## Installation

```bash
hermes plugins install GottZ/ctx/integrations/hermes/plugin/ctx_discord_policy --enable
```

Restart the gateway afterwards. A plugin update also needs a gateway restart: an instance that finds an older
version already running in the process registers nothing and reports "restart required".

### Activation per profile

The plugin protects a turn only in a profile where it is enabled. With `multiplex_profiles` and
`profile_routes`, enable it in **every profile a Discord server routes to**, not only in the profile that runs
the bot:

```bash
hermes -p <profile> plugins enable ctx_discord_policy
```

A server that routes to a profile without the plugin stays closed for members (they are not admitted), and the
status names that profile's home directory. Operators keep Hermes' normal behaviour there.

## Operators

Operators are Discord user IDs in the Hermes configuration of the profile that runs the bot:

```bash
hermes config set plugins.entries.ctx_discord_policy.settings.operators '["<your Discord user id>"]'
```

You can also set them in the Plugins hub of Hermes Desktop or the TUI. Changes apply to the next message without
a restart. Without an operator, nobody can activate the policy from Discord (`hermes ctx-discord-policy bind` on the
host still can), and nobody's turn gets tools.

Other settings in the same place: `frame_prompt` (the operator frame placed before every server's guidance),
`operator_label`, `guild_prompt_max_chars` (upper bound for a server's guidance), `notify_operators` (direct
messages to operators), `locked_profiles` (Hermes profiles never opened to server members and never offered
in `/policy`) and `member_search` (`auto` or `off`: suggest server members by name on the settings page).

## Settings page in Hermes Desktop

The plugin brings its own page: **Settings → Plugins → ctx_discord_policy** with the sub-pages **Servers**,
**Operators**, **Status** and **Activity**, and Hermes' own form of the settings above as **Agent settings**.
It edits the same server settings as `/policy` (one save per section, with a conflict notice when someone
changed the server meanwhile), the server guidance with preview and history, the operator list, and a server's
settings as a file to download or import. A server's Hermes profile and models are shown there but chosen in
`/policy`.

The page needs Hermes Desktop 0.23.0 or newer (0.22.4 does not install a plugin's desktop half). Turn on the
plugin's desktop half under **Capabilities → Plugins** after a rescan (Hermes keeps it off until you do), and
restart Hermes Desktop once after enabling the plugin so its backend is mounted. The page asks
Hermes' dashboard backend, behind Hermes' own sign-in and origin checks, never Discord directly. Channel, role
and emoji names come from the running gateway: read live when the backend runs in the gateway process, else
from the server directory the gateway keeps in the profile's plugin data (or over Hermes' plugin RPC, from
Hermes 04846730f3 on). Without either, the page takes IDs.

## Inviting the bot

The bot needs the `bot` and `applications.commands` scopes (`/policy` is a per-server slash command):

```
https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot+applications.commands&permissions=309237763136
```

The permissions are View Channels, Send Messages, Send Messages in Threads, Create Public Threads (worklog
threads), Read Message History, Embed Links, Attach Files and Add Reactions. A bot that is already in a server
without `applications.commands` can be invited again with this URL; nothing else changes.

## Activating a server

In the server, run `/policy` and choose **Activate in this server** (operators only). Server managers who are
not operators can ask the operator to activate it from the same panel. On the host the same works with:

```bash
hermes ctx-discord-policy bind <server id>          # activate
hermes ctx-discord-policy pause <server id>         # stock Hermes behaviour in that server
hermes ctx-discord-policy unbind <server id> [--purge]
hermes ctx-discord-policy status                    # servers, operators, and the status of every intervention
hermes ctx-discord-policy export [--guild <id>] > server.json
hermes ctx-discord-policy import server.json        # replace one server's settings from an export file
hermes ctx-discord-policy routing list              # each server's profile: chosen, configured, exceptions
hermes ctx-discord-policy routing set <server id> <profile>
hermes ctx-discord-policy routing clear <server id> # back to the bot's own profile
hermes ctx-discord-policy routing adopt <server id> [--entry NAME]   # take over a hand-written guild route
```

`--bot <application id>` selects the bot when the host runs more than one. A `routing` change made on the host
applies to the running gateway after a restart, or right away with **Apply again** in `/policy` → Profile;
members' messages in that server stay closed until then.

`/policy` needs the Manage Server permission by default; server admins can allow it for other members or roles
under Server Settings → Integrations. Who may change which setting is decided by the plugin on every click.

## Compatibility

| Hermes | Tested | Notes |
|---|---|---|
| `main` | daily and on every change | drift is reported as an issue in this repository |
| latest three releases | daily | security drift turns the run red |
| v0.23.0 | on every change | `delete_message` and per-server `rotate` are added by the plugin |
| v0.22.4 | on every change | oldest supported version |

Where Hermes later gains a feature itself (deleting progress messages, rotating progress lines), the plugin
detects it and steps back; the status then shows `native` instead of `injected` or `wrapped`.

## License

MPL-2.0, see [LICENSE](LICENSE).
