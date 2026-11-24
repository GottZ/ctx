"""Settings fields of the memory form (Hermes Desktop: Settings → Memory → ctx_checkpoint).

``FIELDS`` is the one field table both memory forms are built from: Hermes Desktop reads
``CONFIG_SCHEMA`` below (``?surface=declared``; the provider reads and writes the values
through ``read_config_fields`` / ``write_config_fields``), the web dashboard and ``hermes
memory setup ctx_checkpoint`` read ``get_config_schema()`` in ``__init__.py``, which derives
its fields from the same table. Hermes loads this file by path, so it may import only
``plugins.memory.config_schema``; defaults are literals, and a unit test keeps them equal to
the plugin's ``_DEFAULT_*`` values. The keys of the Settings → Plugins form live in
``plugin.yaml`` (``config_schema``); no key is in both forms.
"""

from __future__ import annotations

PROVIDER_NAME = "ctx_checkpoint"
DOCS_URL = "https://github.com/GottZ/ctx/blob/root/integrations/hermes/plugin/ctx_checkpoint/README.md"

# kind: text | secret | select | number | bool. "inline" fields sit in the compact panel, the
# others in "Full config…" by group. "required", "url", "minimum", "maximum" and "step" are read
# by the terminal setup and the web dashboard only. "desktop_only" fields are left out of
# get_config_schema(): the web dashboard sends every field with the value it loaded, so a value
# changed elsewhere in the meantime would be written back unnoticed (terminal setup asks for the
# operating mode on its own, after the connection test).
FIELDS: tuple[dict, ...] = (
    {
        "key": "ctx_url",
        "kind": "text",
        "label": "ctx server URL",
        "description": (
            "Address of your ctx server's MCP endpoint, ending in /mcp. Use https:// unless the server runs "
            "on this computer or in your local network: http:// sends the key and your conversations "
            "unencrypted."
        ),
        "placeholder": "https://ctx.example.com/mcp",
        "required": True,
        "inline": True,
        "group": "Connection",
    },
    {
        "key": "ctx_api_key",
        "kind": "secret",
        "label": "ctx API key",
        "description": (
            "API key for that ctx server, stored in Hermes' .env as CTX_API_KEY. Saving the URL or the key "
            "tests the connection; a failed test saves nothing and tells you why."
        ),
        "env_key": "CTX_API_KEY",
        "required": True,
        "url": "https://github.com/GottZ/ctx#readme",
        "inline": True,
        "group": "Connection",
    },
    {
        "key": "sensitivity",
        "kind": "select",
        "label": "Sensitivity",
        "description": (
            "How sensitive the saved conversation is. ctx hands a block only to the models its operator "
            "allows for that level; a higher level allows fewer models."
        ),
        "options": (
            ("public", "public"),
            ("internal", "internal"),
            ("personal", "personal"),
            ("credentials", "credentials"),
        ),
        "default": "internal",
        "group": "Checkpoint storage",
    },
    {
        "key": "category",
        "kind": "text",
        "label": "Category",
        "description": "ctx category the saved conversation is filed under.",
        "default": "compaction-checkpoints",
        "group": "Checkpoint storage",
    },
    {
        "key": "chunk_chars",
        "kind": "number",
        "label": "Part size (characters)",
        "description": "The saved conversation is split into parts of at most this many characters (1,000 to 40,000).",
        "default": 36000,
        "minimum": 1000,
        "maximum": 40000,
        "step": 1000,
        "group": "Checkpoint storage",
    },
    {
        "key": "memory",
        "kind": "bool",
        "label": "Memory features",
        "description": (
            "On: before each reply Hermes recalls what ctx knows, gets the ctx_* tools, copies MEMORY.md and "
            "USER.md entries to ctx and saves facts and session summaries. Off: only the conversation is "
            "saved before each compaction. The single features are switched under Settings → Plugins → "
            "ctx_checkpoint."
        ),
        "default": False,
        "inline": True,
        "group": "Mode",
    },
    {
        "key": "mcp_grant",
        "kind": "bool",
        "label": "Let Hermes end a ctx call the checkpoint gives up on",
        "description": (
            "On: the checkpoint's ctx calls go through Hermes' call_mcp with the checkpoint's own time limit, so a "
            "call it gives up on ends in Hermes as well instead of holding the ctx server entry until the next "
            "checkpoint's first call fails (writes plugins.entries.ctx_checkpoint.mcp_allowlist; the plugin reaches "
            "the same ctx server either way)."
        ),
        "default": False,
        "group": "Connection",
    },
    {
        "key": "checkpoint_required",
        "kind": "bool",
        "label": "Wait for the checkpoint before compacting",
        "description": (
            "Hermes' setting compression.checkpoint_required. On: Hermes compacts a conversation only after "
            "ctx confirmed the saved copy; while ctx cannot be reached nothing is compacted, and a session "
            "that needs compaction cannot go on. Off (Hermes' default): Hermes compacts anyway "
            "when saving fails, and some compactions (gateway hygiene, /compress in a chat channel, "
            "sub-agents, the curator) never save a copy. Turning it on tests the connection first."
        ),
        "default": False,
        "inline": True,
        "group": "Mode",
        "desktop_only": True,
    },
)

try:  # Hermes without the declared memory surface: FIELDS still serves get_config_schema()
    from plugins.memory.config_schema import (
        KIND_BOOL,
        KIND_NUMBER,
        KIND_SECRET,
        KIND_SELECT,
        KIND_TEXT,
        STORAGE_PROVIDER,
        ProviderConfigSchema,
        ProviderField,
        ProviderFieldOption,
    )
except ImportError:  # pragma: no cover - depends on the host version
    CONFIG_SCHEMA = None
else:
    _KINDS = {
        "text": KIND_TEXT,
        "secret": KIND_SECRET,
        "select": KIND_SELECT,
        "number": KIND_NUMBER,
        "bool": KIND_BOOL,
    }

    def _declared_default(spec: dict) -> str:
        default = spec.get("default", "")
        if isinstance(default, bool):
            return "true" if default else "false"
        return str(default)

    CONFIG_SCHEMA = ProviderConfigSchema(
        name=PROVIDER_NAME,
        label=PROVIDER_NAME,
        storage=STORAGE_PROVIDER,
        docs_url=DOCS_URL,
        fields=tuple(
            ProviderField(
                key=spec["key"],
                label=spec["label"],
                kind=_KINDS[spec["kind"]],
                default=_declared_default(spec),
                description=spec.get("description", ""),
                placeholder=spec.get("placeholder", ""),
                options=tuple(ProviderFieldOption(value, label) for value, label in spec.get("options", ())),
                env_key=spec.get("env_key"),
                inline=bool(spec.get("inline")),
                group=spec.get("group", ""),
            )
            for spec in FIELDS
        ),
    )
