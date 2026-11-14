<p align="center"><img src="../../assets/ctx_discord_policy.webp" alt="ctx discord policy: everyone may talk, only you hold the keys" width="100%"></p>

# ctx_discord_policy

**Everyone may talk. Only you hold the keys.**

`ctx_discord_policy` lets a [Hermes](https://github.com/NousResearch/hermes-agent) bot take part in the
conversation on your Discord servers without handing its tools to everyone there. Members of a server can call
the bot by name, ask it something, chat with it in a channel you open for that, and get an answer. When you
write to it, it works for you as usual: it reads files, runs commands, uses your MCP servers. Nobody else gets
that, not a server admin, not a role, not a clever message. Each server is set up from inside Discord with
`/policy`, and the server's own managers can do the parts you leave to them.

```
/policy                                                       in a server: activate it, its settings, its status
hermes ctx-discord-policy status                              on the host: servers, operators, what is protected
hermes ctx-discord-policy bind <server id>                    on the host: activate a server without Discord
hermes ctx-discord-policy routing set <server id> <profile>   on the host: run a server in another profile
```

## What you get

- **Tools only for you.** Who may use tools (the operators) is a list of Discord user IDs in the Hermes
  configuration on the host. Nothing in Discord can extend it: no role, no server admin, no guidance text.
- **Members can talk to the bot.** They mention it, reply to it, call it by one of its names ("Nova, …",
  "hey Nova"), or react to a message with the server's wake emoji; in a conversation channel the bot reads along
  and joins in when it has something to add. Their turns run without tools, except the member tools you allow
  for that server: the current channel's history, linked messages, skills you name, entries of your knowledge
  sources you name.
- **Links and channels in a message.** When a message links Discord messages or mentions channels, the bot sees
  that content as quoted data, read with the rights of the person who wrote the message. In members' turns only
  what everyone who sees the answer may read.
- **Every server set up in Discord.** `/policy` opens panels for the bot's name, wake names and reply language,
  the channels where it joins in or keeps quiet, a guidance text for the server, the progress display, the
  worklog, what members may let it do, who of the server may change what, and the Hermes profile and models the
  server runs on.
- **Your tool work out of the way.** With a worklog channel, the tool work of your turns goes into a thread of
  its own; the channel you wrote in gets a pointer and, if you want, a copy of the final answer.
- **You hear about changes.** An audit channel gets one line per change made in `/policy` (who, what, never the
  values). You get a direct message when a server asks for activation, when its guidance changes, when
  protection gets worse and when a model chosen for a server cannot be used.
- **A settings page in Hermes Desktop.** Settings → Plugins → ctx_discord_policy: your servers, the operators,
  the status and the change history, next to Hermes' own form for the plugin settings.
- **It checks itself against your Hermes.** Every place where the plugin hooks into Hermes is tested when it
  starts. Works without ctx.

## How it works

The plugin runs inside the Hermes gateway, next to the Discord platform. For every message in a server where
you activated it:

| Step | What happens |
|---|---|
| 1. Who is it | The author is an operator (in your Hermes configuration) or a member (anyone else). Bots, webhooks and system messages never wake the bot. |
| 2. Does the bot answer | A message gets a turn when it mentions or replies to the bot, calls it by a wake name, or is in a conversation channel; a reaction with the wake emoji gets the reactor an answer to that message. Never in a channel you excluded. Everything is checked first: the server is active, the profile the message goes to has the plugin enabled and is the one you chose, every safety hook is in place. If anything is missing, members get no turn. |
| 3. What the turn may do | An operator's turn runs as Hermes would run it. A member's turn gets only the member tools of that server. |
| 4. What the model is told | Before the turn the plugin adds a short context: your frame, what this turn may use, how to behave in the conversation, the server's guidance and linked content, all of it as quoted text, never as instructions about tools. |
| 5. Every tool call | is checked against the exact message it belongs to. A call outside what that turn may use, or from a turn the plugin cannot tie to its message, is refused ("BLOCKED by ctx_discord_policy"). |
| 6. Delivery | An operator's tool work goes to the worklog thread when the server has one; progress lines follow the server's progress setting. |

When Hermes itself later gains something the plugin adds today (deleting progress messages, one progress line
that keeps being replaced), the plugin notices and steps back: the status then reads `native` instead of
`injected` or `wrapped`. Safety checks that cannot be made close the server for members; comfort features fall
back to Hermes' own behaviour.

## Before you start

- Hermes 0.22.4 or newer with the Discord platform. The settings page needs Hermes Desktop 0.23.0 or newer.
- Plugins running in the gateway process, Hermes' default. Under `plugins.isolation: host` the plugin stays
  inactive and says so in the status.
- A Discord application with the **Message Content** intent (Hermes needs it as well).
- One session per member: with `group_sessions_per_user: false` Hermes would put several members of a channel
  into one session, so the plugin keeps the server closed for members.
- The test suites run on Linux, against Hermes 0.22.4, 0.23.0 and upstream `main`; the settings page is tested
  in a browser emulation, not yet in Hermes Desktop itself.

What does not work, or only partly:

- **The settings page** shows a server's Hermes profile and models but cannot change them; choose them in
  `/policy`.
- **Names on the settings page.** Channel, role and member names come from the running gateway: read directly
  when the page's backend runs in the gateway process, else from the server directory the gateway keeps, or
  over Hermes' plugin RPC (Hermes 0.23.1 or newer). Without either you enter IDs.
- **A profile chosen on the host** with `hermes ctx-discord-policy routing set` reaches the running gateway at
  its next start, or at once with **Apply again** in `/policy` → Profile. Until then members get no answer in
  that server.
- **The Status page** shows the current status; its history stays empty, nothing writes it yet. Per-turn
  notices reach `hermes ctx-discord-policy status` only after the bot connects again.

## Install

```bash
hermes plugins install GottZ/ctx/integrations/hermes/plugin/ctx_discord_policy --enable
```

Restart the gateway afterwards. An update also needs a gateway restart: a newer copy that finds the older one
still running in the process registers nothing and reports "restart required".

The plugin protects a turn only in a profile where it is enabled. With `multiplex_profiles` and
`profile_routes`, enable it in **every profile a Discord server routes to**, not only in the profile that runs
the bot:

```bash
hermes -p <profile> plugins enable ctx_discord_policy
```

A server that routes to a profile without the plugin stays closed for members, and the status names that
profile. Operators keep Hermes' normal behaviour there.

For the settings page, turn on the plugin's desktop half in Hermes Desktop under Capabilities → Plugins, and
restart Hermes Desktop once so the page's backend is mounted.

## Set up once

1. **Operators.** Put your Discord user ID into the configuration of the profile that runs the bot:

   ```bash
   hermes config set plugins.entries.ctx_discord_policy.settings.operators '["<your Discord user id>"]'
   ```

   Or in Hermes Desktop under Settings → Plugins → ctx_discord_policy. A change applies to the next message,
   without a restart. Without an operator nobody can activate a server from Discord, and nobody's turn gets
   tools.
2. **Invite the bot** with the `bot` and `applications.commands` scopes (`/policy` is a slash command per
   server):

   ```
   https://discord.com/oauth2/authorize?client_id=<APPLICATION_ID>&scope=bot+applications.commands&permissions=309237763136
   ```

   The permissions are View Channels, Send Messages, Send Messages in Threads, Create Public Threads (worklog
   threads), Read Message History, Embed Links, Attach Files and Add Reactions. A bot that is already in a
   server without `applications.commands` can be invited again with this URL; nothing else changes.
3. **Activate a server.** In the server, run `/policy` and choose **Activate in this server** (operators only).
   Server managers who are not operators can ask you from the same panel; you get a direct message.

Further settings of the plugin (Hermes Desktop shows them as a form): `frame_prompt` (your text before every
server's guidance), `operator_label`, `guild_prompt_max_chars` (the longest guidance text of a server),
`notify_operators` (the direct messages), `locked_profiles` (profiles members never reach) and
`member_search`.

## Everyday use

| You want to | Where | Command |
|---|---|---|
| Change a server's settings | in the server | `/policy`, then the panel |
| See what is protected and what fell back | in the server, or on the host | `/policy` → Status, `hermes ctx-discord-policy status` |
| Run a server in another Hermes profile | in the server, or on the host | `/policy` → Profile, `hermes ctx-discord-policy routing set <server id> <profile>` |
| See each server's profile | on the host | `hermes ctx-discord-policy routing list` |
| Pause the plugin in a server (Hermes' own behaviour there) | on the host | `hermes ctx-discord-policy pause <server id>` |
| Activate it again | on the host | `hermes ctx-discord-policy bind <server id>` |
| Remove a server, with its settings | on the host | `hermes ctx-discord-policy unbind <server id> --purge` |
| Keep a copy of a server's settings, or move them | on the host, or on the settings page | `hermes ctx-discord-policy export --guild <server id>`, `… import <file>` |

`--bot <application id>` picks the bot when the host runs more than one. `/policy` needs the Manage Server
permission by default; server admins can open it to other members or roles under Server Settings →
Integrations. Which setting someone may change is decided by the plugin on every click.

**Profile per server.** A server can run in another Hermes profile, with that profile's tools, memory and
credentials. The plugin writes a named route into `profile_routes` of the default profile's `config.yaml` and
applies it to the running gateway; where it may not write `config.yaml`, it applies the route again at every
start. Members get an answer only while the running gateway sends the server to the chosen profile, the plugin
is enabled there and the profile is not in `locked_profiles`. Offered are profiles that Hermes serves with the
plugin enabled and that run no Discord bot of their own.

**Models per server.** Members' turns and your own turns in a server can each use another model of the
server's profile. It is used only where that profile has a credential for the provider and the conversation has
no `/model` or `channel_overrides` entry of its own; otherwise Hermes' choice stands, or, if you set the server
so, members get no answer until the model works again.

## Safety in short

- **Authority lives on the host.** Operators come from the Hermes configuration only; the plugin's own files
  hold server settings, never authority.
- **Fail closed.** If the policy file is damaged, every server is paused. If a safety check cannot be made,
  members get no turn. A tool call the plugin cannot tie to its message is refused.
- **Guidance and linked content are data.** A server's guidance text and the content of linked messages are
  quoted in the turn; they cannot open tools or change who is an operator.
- **Reading follows the author's rights.** Linked messages and mentioned channels are read only from the same
  server and only where the author can read them. A member's turn reads only what everyone who sees the answer
  may read; your own turns read linked content only when you address the bot. Unaddressed messages are not
  read further.
- **Member turns stay out of the worklog.** A member's turn never runs in the worklog channel or its threads
  and never delivers there. Who can read that channel is up to its Discord permissions.

### Disclosure

- **Network:** the plugin itself talks only to Discord: through the bot Hermes already runs, and with the
  bot's token for the member search of the settings page (`member_search: auto`) when the gateway cannot
  answer it. The *Models* panel lists models through Hermes' own model picker, which may ask your configured
  providers for their model lists. No other services, no telemetry, no update checks.
- **Stored:** the server settings in `plugin-data/ctx_discord_policy/` of the profile that runs the bot
  (`policy.json`, readable by you alone, with a list of the last 500 changes that holds only hashes of the old
  and new values), the server directory for the settings page (names of servers, channels, roles and emojis,
  never members), and small state files next to it (status, activation requests, bookkeeping of notifications
  and of the `/policy` command per server).
- **Hermes configuration:** the plugin writes `plugins.entries.ctx_discord_policy.settings` only from the
  settings page, and a server's route into `profile_routes` when you choose a profile for it.
- **Inbound:** the settings page's routes under `/api/plugins/ctx_discord_policy/`, behind the dashboard
  sign-in of Hermes, and read-only plugin RPC methods on the gateway's own session socket.

## Compatibility

| Hermes | Tested | Notes |
|---|---|---|
| `main` | daily and on every change | drift is reported as an issue in [GottZ/ctx](https://github.com/GottZ/ctx/issues?q=label%3Actx_discord_policy) with the label `ctx_discord_policy` |
| latest three releases | daily | security drift turns the run red |
| v0.23.0 | on every change | `delete_message` and the per-server progress line are added by the plugin |
| v0.22.4 | on every change | oldest supported version |

## License

MPL-2.0, see [LICENSE](LICENSE). Part of [ctx](https://github.com/GottZ/ctx); ctx is a separate knowledge store,
and this plugin works without it.
