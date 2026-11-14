"""HTTP backend of the Hermes settings page (SETTINGS-PAGE §2, §3.4), framework-free.

``dashboard/plugin_api.py`` (loaded by Hermes' dashboard mount, behind its auth gate) carries these handlers
with FastAPI; nothing here imports FastAPI or Hermes at import time. The page acts as the host role and
writes only through ``ui.perms``, ``store.ops`` and ``PolicyStore.update`` — there is no second write path.
"""
