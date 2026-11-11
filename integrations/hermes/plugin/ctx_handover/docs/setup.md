# Set up ctx_handover

[← ctx_handover](../README.md)

Pair your two machines once. After that, every handover is one command ([Use](usage.md)).

## Before you start

You need:

- Hermes 0.22.6 or newer on both machines, and `git` 2.38 or newer if your project is a git repository
  (`/handover doctor` checks the git version).
- A web address under which your laptop reaches your server's Hermes dashboard (the web page of your server's Hermes) —
  the address you open the dashboard with in a browser, such as `https://server.example.net`. A plain `http://` address
  works too if only your own devices can reach it, through a private network such as Tailscale.
- The sign-in of your server's dashboard: a fixed dashboard token (step 4 below says what to do), or a user name
  and password. `/handover setup` on the server tells you which one.

If you do not know the address or how your server's Hermes is set up, ask whoever set it up — or use the handover file
([Use, section 3](usage.md#3-without-a-connection-use-a-handover-file)), which needs neither (the plugin must still be installed on both machines, step 1 below).

The plugin is tested on Linux; on macOS and Windows it has had no end-to-end runs yet.

What does not work, or only partly:

- If Hermes Desktop reaches your server only through an SSH tunnel, a Nous/OAuth sign-in or Hermes Cloud, the plugin has
  no sign-in it can use. Then only the handover file works ([Use, section 3](usage.md#3-without-a-connection-use-a-handover-file)). Not sure? Go through the pairing steps below: if the laptop
  cannot reach the server, pairing fails with a message, and the handover file still works.
- The laptop always connects to the server, never the other way round. A machine that the other one cannot reach over
  the network (for example a home computer that is not reachable from outside) can only get a handover file.
- Running programs, subagents and scheduled (cron) jobs do not move. The agent on the other machine gets a note that
  names them.
- If both copies of a session go on, they are never merged.

Every limit, with the details, is listed under [Limits](limits.md).

## Pair your laptop and your server (once)

Pair the two machines once so they trust each other. You type the `/handover` commands below into a Hermes chat on the
machine named. A chat "on the server" is one that runs there: `hermes` in a terminal on the server, or Hermes Desktop
with the server's gateway chosen instead of *This device*. Chats in messaging apps refuse most of these commands.

**On both machines**

1. Install and enable the plugin: in Hermes Desktop under Capabilities → Plugins (once for the server's gateway and once
   for *This device*; the plugin's settings form is a separate page, Settings → Plugins → ctx_handover), or in a
   terminal on each machine:

   ```
   hermes plugins install ctx_handover
   hermes plugins enable ctx_handover
   ```

**On your server**

2. Tell the plugin the address your laptop uses to reach the server — the address of the server's Hermes dashboard (see
   "Before you start"). In Settings → Plugins → ctx_handover, fill *Address other devices use to reach this gateway*
   (`listen_url`), or run in a terminal on the server:

   ```
   hermes config set plugins.entries.ctx_handover.settings.listen_url https://server.example.net
   ```

3. Restart Hermes on the server, so that it answers the laptop: restart the gateway with `hermes gateway restart`. If
   the dashboard runs as its own program (`hermes serve`), restart that too. Without the restart, the laptop later
   reports that the server "has no ctx_handover routes" (the plugin's addresses on the server are missing).
4. In a chat on the server, run:

   ```
   /handover setup
   ```

   It checks the address and tells you how the server's dashboard signs people in:
   - *with a fixed token*: the server needs a fixed sign-in token. You choose it yourself: any long random value (for
     example the output of `openssl rand -hex 32`). Set it as `HERMES_DASHBOARD_SESSION_TOKEN` in the environment that
     starts the server's gateway and `hermes serve` (for example their service settings), then restart both. Your laptop
     needs this same value in step 5.
   - *with a Basic login*: your laptop needs the dashboard's user name and password in step 5.

   If you do not use ctx, `/handover setup` says so. If it asks whether to record handovers in ctx, either answer is
   fine — handovers work without it.

**On your laptop**

5. In Settings → Plugins → ctx_handover, enter the server's sign-in: *Dashboard token of the other gateway*
   (`peer_token`). For a Basic login, set *How to pass the other gateway's dashboard sign-in* (`peer_gate`) to
   `basic_login` and fill in *Dashboard user name* and *Dashboard password*. Hermes keeps the token and the password in
   this profile's `.env` file, never in `config.yaml`.

**On your server**

6. Create a pairing line:

   ```
   /handover pair --name laptop
   ```

   `--name laptop` is the name the server gives your laptop. The answer shows one line that starts with
   `/handover pair ctxh-pair:`. That line holds the server's address, a one-time code (valid for 10 minutes, usable
   once) and a fingerprint of the server's key — a check value the laptop compares, so that it talks to the right
   server. It never holds your dashboard sign-in. Treat it like a password until it is used: copy it straight into the
   laptop chat, and do not post it anywhere. If it has expired, run this step again.

**On your laptop**

7. Copy that whole line into a chat on the laptop and send it:

   ```
   /handover pair ctxh-pair:…
   ```

   If the server's address starts with `http://` (not `https://`), add `--allow-insecure` at the end — only for an
   address that just your own devices can reach. The answer starts with "Paired with" and the name the laptop gave the
   server — the server's computer name (its host name); this README writes `server`. Use that name in the commands
   below; `/handover peers` on the laptop lists it.
8. Run `/handover doctor` in a chat on each machine. It checks the setup and says what to fix.

By default, pairing lets the laptop do both: take sessions from the server and send sessions to it. To allow only one,
add `--rights pull` (take) or `--rights push` (send) in step 6.

## Set up in detail

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
   variables of that gateway alone (`CTX_HANDOVER_PEER_TOKEN_<peer id>`, see [Disclosure](security.md#disclosure)), so the form then takes the
   next gateway's credential; to change a paired gateway's credential later, edit its variable (Keys page of the
   dashboard or `.env`; `/handover doctor` names it) or pair again.
4. **Pair:** in a chat on the server run `/handover pair --name laptop`. Copy the line it prints,
   `/handover pair ctxh-pair:…`, and run it in a chat on the laptop. The `ctxh-pair:` string carries the address, a
   one-time code (10 minutes, single use) and the server's key fingerprint, which the laptop checks itself — it
   never carries a dashboard credential. For a plain-HTTP address that only your VPN reaches (e.g. Tailscale) add
   `--allow-insecure`. The laptop names the server as the line does, or takes the next free name (`server-2`) when
   that one is taken there; with `--name` you choose it, and a taken name is refused before the code is used.
5. `/handover doctor` on both sides checks the setup and says what to fix. `/handover doctor --measure` also finds
   the request-body limit of a proxy in front of each paired gateway (test uploads, see [Disclosure](security.md#disclosure)).
