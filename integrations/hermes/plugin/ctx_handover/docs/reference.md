# Reference

[← ctx_handover](../README.md)

## Words used in these docs

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

## Settings

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

## Troubleshooting

- **`hermes handover` or `/handover` is unknown on a Hermes installed from a source checkout.** Such an
  installation reads its own version with `git tag --merged HEAD` under a 3-second limit; on a busy machine the
  limit runs out, Hermes falls back to an older base version, and this plugin (it needs 0.22.6 or newer) is skipped
  in that process — the log says `Plugin 'ctx_handover' skipped: requires hermes >=0.22.6`, and
  `hermes plugins enable ctx_handover` can be refused the same way. Run the command again, or restart the gateway,
  once the machine is less busy. Packaged installations (Docker, Nix) carry a version stamp and are not affected.
