"""Modals: one declarative spec, two renderings (DESIGN §6.2 K6).

* ``modern`` (discord.py with ``Label``/``RadioGroup``/``Checkbox``/``FileUpload``): every field sits in a
  ``Label`` with a description; radio groups and checkboxes are available.
* ``classic``: text inputs in plain rows only. Panels offer choices as selects in the message instead.

A modal is not persistent (Discord drops open modals with the process, DESIGN §12). Its ``custom_id`` is
unique per opening (discord.py keys open modals by ``custom_id`` alone), and on submit the server, the
submitting user, the caller's rights and the settings revision are all checked again.
"""

from __future__ import annotations

import logging
import os
from dataclasses import dataclass, field
from typing import Any, Optional

from . import items

logger = logging.getLogger("ctx_discord_policy")

TITLE_MAX = 45
FIELDS_MAX = 5
MODAL_TIMEOUT = 1800.0


@dataclass(frozen=True)
class Option:
    label: str
    value: str
    description: str = ""


@dataclass
class Field:
    key: str
    kind: str                       # "text" | "paragraph" | "radio" | "checkbox"
    label: str
    description: str = ""
    value: Any = None               # prefilled text, preselected option value or checkbox state
    options: tuple = ()
    required: bool = True
    min_length: Optional[int] = None
    max_length: Optional[int] = None
    placeholder: Optional[str] = None


@dataclass
class ModalSpec:
    action: str                     # modal action that receives the values
    title: str
    fields: list = field(default_factory=list)

    def check(self, *, modern: bool) -> None:
        if not 1 <= len(self.title) <= TITLE_MAX:
            raise ValueError(f"modal title must be 1..{TITLE_MAX} characters: {self.title!r}")
        if not 1 <= len(self.fields) <= FIELDS_MAX:
            raise ValueError(f"a modal holds 1..{FIELDS_MAX} fields")
        for f in self.fields:
            if f.kind not in ("text", "paragraph", "radio", "checkbox"):
                raise ValueError(f"unknown field kind {f.kind}")
            if not modern and f.kind not in ("text", "paragraph"):
                raise ValueError(f"field {f.key}: {f.kind} needs the modern modal components")


def _component(discord: Any, f: Field, *, modern: bool) -> Any:
    if f.kind in ("text", "paragraph"):
        style = discord.TextStyle.paragraph if f.kind == "paragraph" else discord.TextStyle.short
        kwargs: dict = {"custom_id": f.key, "style": style, "required": f.required}
        if f.value not in (None, ""):
            kwargs["default"] = str(f.value)
        if f.min_length is not None:
            kwargs["min_length"] = f.min_length
        if f.max_length is not None:
            kwargs["max_length"] = f.max_length
        if f.placeholder:
            kwargs["placeholder"] = f.placeholder
        if not modern:
            kwargs["label"] = f.label[:45]
        return discord.ui.TextInput(**kwargs)
    if f.kind == "radio":
        group = discord.ui.RadioGroup(custom_id=f.key, required=f.required)
        for opt in f.options:
            group.add_option(label=opt.label, value=opt.value, description=opt.description or None,
                             default=opt.value == f.value)
        return group
    return discord.ui.Checkbox(custom_id=f.key, default=bool(f.value))


_CLASSES: dict = {}


def modal_class(discord: Any) -> Any:
    cls = _CLASSES.get(id(discord))
    if cls is not None:
        return cls

    class PolicyModal(discord.ui.Modal):
        def __init__(self, *, spec: ModalSpec, guild_id: str, rev: int, opener_id: str, modern: bool) -> None:
            nonce = os.urandom(4).hex()
            super().__init__(title=spec.title, custom_id=items.encode(guild_id, spec.action, nonce, rev),
                             timeout=MODAL_TIMEOUT)
            self.spec = spec
            self.guild_id = str(guild_id)
            self.rev = rev
            self.opener_id = str(opener_id)
            self.inputs: dict = {}
            for f in spec.fields:
                component = _component(discord, f, modern=modern)
                self.inputs[f.key] = component
                if modern:
                    self.add_item(discord.ui.Label(text=f.label[:45], component=component,
                                                   description=(f.description or None)))
                else:
                    self.add_item(component)

        def values(self) -> dict:
            return {key: getattr(component, "value", None) for key, component in self.inputs.items()}

        async def interaction_check(self, interaction: Any, /) -> bool:
            return True  # checked in on_submit, so a refusal gets an answer

        async def on_submit(self, interaction: Any) -> None:
            binding = items.binding_for(getattr(interaction, "client", None))
            if binding is None:
                await interaction.response.send_message(
                    "The policy interface is not ready on this bot yet; nothing was saved.", ephemeral=True)
                return
            await binding.on_modal(interaction, self)

        async def on_error(self, interaction: Any, error: Exception) -> None:
            logger.error("ctx_discord_policy: modal %s failed", self.spec.action, exc_info=error)
            try:
                if not interaction.response.is_done():
                    await interaction.response.send_message(
                        "Something went wrong; nothing was saved. Next step: run /policy again.", ephemeral=True)
            except Exception:  # the interaction may have expired
                pass

    _CLASSES[id(discord)] = PolicyModal
    return PolicyModal


def build(discord: Any, spec: ModalSpec, *, guild_id: str, rev: int, opener_id: str, modern: bool) -> Any:
    spec.check(modern=modern)
    return modal_class(discord)(spec=spec, guild_id=guild_id, rev=rev, opener_id=opener_id, modern=modern)
