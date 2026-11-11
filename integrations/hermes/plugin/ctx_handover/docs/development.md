# Development

[← ctx_handover](../README.md)

## Tests

```bash
# unit suites (fakes, a real SessionDB and real git in temp dirs; the host contract comes from a Hermes checkout)
PYTHONPATH=/path/to/hermes-agent:integrations/hermes/plugin python -m pytest \
    integrations/hermes/plugin/ctx_handover/tests/unit -q
# lint + format + bandit rules (ruff.toml in this directory)
cd integrations/hermes/plugin/ctx_handover && ruff check . && ruff format --check .
# catalog checks
hermes plugins validate integrations/hermes/plugin/ctx_handover
```

`tests/e2e/` drives two real gateways in a lab (two `HERMES_HOME`s, a fake model); it needs that lab running and is
not part of the unit run.

## Layout

```
__init__.py          register(): /handover, hermes handover, four hooks, the handover tool — no I/O
plugin.yaml, VERSION manifest (settings form = config_schema) and the single version source
engine/              the engine; it runs the same in the gateway, hermes serve, the CLI and the plugin host
  commands.py        /handover and hermes handover, hooks, composition with the engine
  surfaces.py        grammar, settings, confirmation, final and error texts
  progress.py        progress notice (post_message), terminal progress
  agent_tool.py      the handover tool
  continuation.py    the detached continuation run
  ctxrecords.py      optional ctx records (outbox)
  …                  session, planning, workspace, transport and orchestration modules
dashboard/           the signed /api/plugins/ctx_handover/v1 routes
tests/               unit, integration and lab suites
```
