"""S1: the policy hooks read only the shared anchor and fail closed."""

from __future__ import annotations


def _route(anchor, lane, **kw):
    routes = anchor.routes
    fields = dict(bot_id="900", guild_id="100", channel_id="33", message_id="44", author_id="7", lane=lane)
    fields.update(kw)
    return routes.Route(**fields)


def test_public_route_blocks_tools_operator_route_keeps_them(anchor):
    hooks, runtime = anchor.hooks, anchor.RUNTIME
    public = _route(anchor, "public")
    token = runtime.route.set(public)
    try:
        assert hooks.on_pre_tool_call(tool_name="terminal", session_id="s", turn_id="t")["action"] == "block"
        assert public.tool_started is False
    finally:
        runtime.route.reset(token)
    operator = _route(anchor, "operator", message_id="45")
    token = runtime.route.set(operator)
    try:
        assert hooks.on_pre_tool_call(tool_name="terminal", session_id="s", turn_id="t2") is None
        assert operator.tool_started is True
    finally:
        runtime.route.reset(token)


def test_turn_binding_recovers_the_exact_route_without_the_contextvar(anchor):
    hooks, runtime = anchor.hooks, anchor.RUNTIME
    operator = _route(anchor, "operator", message_id="1")
    public = _route(anchor, "public", message_id="2")
    for route, turn in ((operator, "op-turn"), (public, "pub-turn")):
        token = runtime.route.set(route)
        try:
            assert "context" in hooks.on_pre_llm_call(session_id="shared", turn_id=turn)
        finally:
            runtime.route.reset(token)
    # ContextVar gone (worker without the copied context): the exact turn decides, not the latest actor.
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="shared", turn_id="pub-turn")["action"] == "block"
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="shared", turn_id="op-turn") is None


def test_public_session_without_exact_route_gets_no_tools(anchor):
    hooks, runtime = anchor.hooks, anchor.RUNTIME
    token = runtime.route.set(_route(anchor, "public"))
    try:
        hooks.on_pre_llm_call(session_id="pub-session", turn_id="t1")
    finally:
        runtime.route.reset(token)
    # A later turn in the same session that arrives without a route (new ingress path, lost context).
    assert hooks.on_pre_llm_call(session_id="pub-session", turn_id="t2") is None
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="pub-session", turn_id="t2")["action"] == "block"
    # An unrelated session without any policy history keeps stock behaviour.
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="other", turn_id="t") is None
    # Hermes fires on_session_end after EVERY turn: the marker must survive it, and finalize/reset too.
    hooks.on_turn_end(session_id="pub-session", turn_id="t1")
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="pub-session", turn_id="t3")["action"] == "block"
    hooks.on_session_gone(session_id="pub-session")
    assert hooks.on_pre_tool_call(tool_name="terminal", session_id="pub-session", turn_id="t4")["action"] == "block"


def test_turn_end_forgets_only_its_own_binding(anchor):
    hooks, runtime = anchor.hooks, anchor.RUNTIME
    for turn in ("t1", "t2"):
        token = runtime.route.set(_route(anchor, "operator", message_id=turn))
        try:
            hooks.on_pre_llm_call(session_id="s", turn_id=turn)
        finally:
            runtime.route.reset(token)
    hooks.on_turn_end(session_id="s", turn_id="t1")  # an interrupted turn ends after the next one started
    assert hooks.current_route("s", "t1") is None
    assert hooks.current_route("s", "t2") is not None
    assert dict(hooks.HOOKS)["on_session_end"] is hooks.on_turn_end


def test_armed_live_without_hermes_follows_the_armed_register(anchor, monkeypatch):
    from fakes.ctx import FakeCtx

    home = anchor.tmp / "live"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    key = anchor.mod("hermes").home_key(home)
    assert not anchor.hooks.armed_live(key)
    anchor.entry.register(FakeCtx("default"))
    assert anchor.hooks.armed_live(key)  # no Hermes registry to consult in unit tests
    monkeypatch.setattr(anchor.mod("hermes"), "hook_registered", lambda *a: False)
    assert not anchor.hooks.armed_live(key)  # registry says the hook is not there: not armed


def test_session_id_alone_never_recovers_a_route(anchor):
    hooks, runtime = anchor.hooks, anchor.RUNTIME
    token = runtime.route.set(_route(anchor, "operator"))
    try:
        hooks.on_pre_llm_call(session_id="s", turn_id="")  # no turn id: nothing recoverable is stored
    finally:
        runtime.route.reset(token)
    assert hooks.current_route("s", "") is None


def test_hook_exceptions_block(anchor, monkeypatch):
    hooks, runtime = anchor.hooks, anchor.RUNTIME

    def boom(*_a, **_k):
        raise RuntimeError("catalog broke")

    monkeypatch.setattr(anchor.mod("policy.lanes"), "public_tool_allowed", boom)
    token = runtime.route.set(_route(anchor, "public"))
    try:
        result = hooks.on_pre_tool_call(tool_name="discord", session_id="s", turn_id="t")
        assert result["action"] == "block" and "failed internally" in result["message"]
    finally:
        runtime.route.reset(token)
    monkeypatch.setattr(anchor.mod("policy.lanes"), "capability_lane_text", boom)
    token = runtime.route.set(_route(anchor, "public"))
    try:
        assert hooks.on_pre_llm_call(session_id="s", turn_id="t")["action"] == "block"
    finally:
        runtime.route.reset(token)
