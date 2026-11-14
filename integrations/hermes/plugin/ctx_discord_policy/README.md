# ctx_discord_policy

Per-server Discord policy for [Hermes](https://github.com/NousResearch/hermes-agent): the bot takes part in
server conversations while tools stay reserved for the operators configured on the host, and each server is
configured from inside Discord with the `/policy` command.

- **Operators only get tools.** Who is an operator comes from the Hermes configuration on the host, never from
  Discord: no role, server admin or server guidance text can grant tools. Members' turns get only the lanes the
  operator enabled for that server (for example reading the current channel's history).
- **Per-server settings in Discord.** Server managers set channels, the bot's name, reply language, progress
  display and a server guidance text; operators additionally set worklog channels and what members may let the
  bot do. Everything is per bot and per server.
- **Adapts to Hermes updates.** Every place where the plugin hooks into Hermes is probed at startup. Safety
  checks fail closed (members get no access) and comfort features fall back to Hermes' own behaviour; the result
  is visible in `/policy` → Status and in `hermes ctx-discord-policy status`. A daily run tests the plugin against
  Hermes `main` and the latest releases.

**Status:** version 3.0.0 is in development and not released yet. In `/policy` today: activating, pausing and
removing a server, activation requests and the progress display; the other settings pages follow.

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
`operator_label`, `guild_prompt_max_chars` (upper bound for a server's guidance) and `notify_operators`
(direct messages to operators about activation requests).

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
```

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
