"""Model per Discord server (PROFILE-PER-GUILD Teil B, layer S13).

Hermes has no per-guild model level and no official way to set the model per source. Intervention K8
wraps ``GatewayRunner._resolve_session_agent_runtime`` (which picks model and runtime) and
``_fallback_baseline_model`` (without it Hermes evicts the agent after every turn) — both or neither.
The fallback is always the model Hermes picks anyway; a session's own ``/model`` and ``channel_overrides``
take precedence.

* :mod:`.check`   — effective model per lane, availability (credential names only), context window;
* :mod:`.runtime` — the K8 wrappers and their installation;
* :mod:`.catalog` — providers and models a profile offers, for the panel.
"""
