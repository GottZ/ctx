# Limits

[← ctx_handover](../README.md)

Everything this version does not do, or does only partly. Words such as *dialer*, *bundle* or *gate* are explained in the [Reference](reference.md#words-used-in-these-docs).

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
- **Your own git filters run without your environment.** git runs with a small fixed set of environment variables
  (see **Commands** under [Disclosure](security.md#disclosure)). A clean filter from your own git configuration that takes its keys from
  environment variables (for example an encryption filter for sops) runs without them while the plugin packs a
  handover. If that filter is marked `required`, the handover fails; otherwise the file goes into the handover copy
  unfiltered, as it lies in the working folder. Filters that read only files under your home or in the repository
  (git-lfs, git-crypt) are not affected.
- **The size check needs the context window there.** Whether the session fits the model on the other side is
  checked against that model's context window, and Hermes gives a plugin only a `context_length` set in that
  gateway's `config.yaml` (`model.context_length`, or `context_length` of the provider entry or of its
  `models.<model>`). Without it nothing is asked about the size — the summary says that the context window there is
  unknown — and the other side compacts an oversized session on its first turn as usual.
- **No relay through ctx.** A handover goes directly between the two gateways or through a bundle file you copy;
  ctx (optional) only keeps a record of it. Relaying a handover through ctx blobs is not built.
- **A repository larger than the other machine allows is refused up front.** The receiving gateway compares the
  planned size with its `max_total_gb` (plus `max_session_mb` for the conversation) when both sides agree on the
  plan, before anything moves; the answer names both numbers. A repository the other machine already has a copy of
  travels as its new commits only and does not count toward that check; one it has never seen counts with its whole
  history — also when that history is larger than the working files, so a project that fits on the disk there can
  still be refused (in 0.1.0 such a handover went through when the disk had room). Then raise `max_total_gb` on the
  receiving machine, or hand over less (`--exclude`, `--no-workspace`).
- **The signature on an answer does not name its request yet.** Every request to a paired gateway is signed over its
  method, path and a one-time value (nonce); the answer is signed over its status, that nonce and its body, not over
  method and path. Someone who can sit between the two machines (a proxy that ends TLS, a peer marked
  `--allow-insecure` over a VPN, a broken dashboard sign-in) could answer a signed request with a genuinely signed
  answer that the gateway gave to another request carrying the same nonce. As far as the code was read, that yields
  at most an error or a hello answer where another answer was expected, never a forged success of a step that
  changes something; it was not run. Planned: 0.2 signs the answer also over method, route path and the gateway's
  identity and pins that per paired device once seen; 1.0 drops the old answer signature.
