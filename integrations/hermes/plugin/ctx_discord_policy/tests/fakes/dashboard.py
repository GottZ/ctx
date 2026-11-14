"""A Hermes stand-in for the settings page backend (``dsp.web.routes.HermesBackend``)."""

from __future__ import annotations

OP = "900000000000000011"


class FakeBackend:
    """What the routes ask Hermes for, recorded and controllable per test."""

    def __init__(self) -> None:
        self.settings: dict = {"operators": [OP]}
        self.unreadable = False
        self.managed = False
        self.writes: list[dict] = []
        self.events: list = []
        self.skills_by_profile: dict = {}
        self.spill = {"enabled": True, "max_chars": 10000}
        self.profile_names: dict = {}
        self.profile_homes: dict = {}
        self.profile_rows = None
        self.model_payload = None

    def read_settings(self) -> dict:
        if self.unreadable:
            raise self.hermes.ConfigUnreadable("config.yaml could not be read")
        return dict(self.settings)

    def write_settings(self, plugin_dir, values) -> list:
        if self.managed:
            raise PermissionError("managed install")
        self.writes.append(dict(values))
        self.settings.update(values)
        return sorted(values)

    def settings_changed(self, written) -> None:
        self.events.append(list(written))

    def spill_config(self):
        return self.spill

    def skills(self, home):
        return self.skills_by_profile.get(str(home))

    def skill_rows(self, home):
        names = self.skills(home)
        return None if names is None else [{"name": n, "description": "", "category": ""} for n in sorted(names)]

    def profile_of(self, home) -> str:
        return self.profile_names.get(str(home), "default")

    def profile_home(self, name):
        return self.profile_homes.get(name)

    def profiles(self):
        return self.profile_rows

    def models(self, profile):
        return self.model_payload
