"""Contract-test harness: run against a real Hermes source checkout.

Set ``HERMES_SRC`` to the checkout (and run with an interpreter that has Hermes' dependencies, including
discord.py). ``HERMES_REF`` names the ref in reports (default: the checkout's directory name);
``CONTRACT_OUT`` is where JSON reports go (default: ``contract-out/`` in the repo). Without ``HERMES_SRC``
every test in this directory is skipped.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
HERMES_SRC = os.environ.get("HERMES_SRC", "").strip()


def pytest_collection_modifyitems(config, items):
    if HERMES_SRC:
        return
    if os.environ.get("DSP_CONTRACT_REQUIRED") == "1":
        pytest.exit("DSP_CONTRACT_REQUIRED=1 but HERMES_SRC is not set: contract tests would be skipped",
                    returncode=2)
    skip = pytest.mark.skip(reason="contract tests need HERMES_SRC (a Hermes source checkout)")
    for item in items:
        if HERE in Path(str(item.fspath)).resolve().parents:
            item.add_marker(skip)


class HermesCheckout:
    def __init__(self, src: Path, ref: str, out: Path, sha: str) -> None:
        self.src = src
        self.ref = ref
        self.out = out
        self.sha = sha

    def write_report(self, name: str, payload: object) -> Path:
        self.out.mkdir(parents=True, exist_ok=True)
        path = self.out / f"{name}-{self.ref.replace('/', '_')}.json"
        if isinstance(payload, dict):
            payload = {"hermes_ref": self.ref, "hermes_sha": self.sha, **payload}
        path.write_text(json.dumps(payload, indent=2, sort_keys=True, default=str) + "\n", encoding="utf-8")
        return path


@pytest.fixture(scope="session")
def hermes(tmp_path_factory) -> HermesCheckout:
    src = Path(HERMES_SRC).resolve()
    if not (src / "hermes_cli").is_dir():
        pytest.fail(f"HERMES_SRC={src} is not a Hermes checkout")
    home = tmp_path_factory.mktemp("hermes-home")
    os.environ["HERMES_HOME"] = str(home)
    os.environ.pop("HERMES_SAFE_MODE", None)
    if str(src) not in sys.path:
        sys.path.insert(0, str(src))
    ref = os.environ.get("HERMES_REF", "").strip() or src.name
    out = Path(os.environ.get("CONTRACT_OUT", "").strip() or REPO / "contract-out")
    sha = os.environ.get("HERMES_SHA", "").strip() or "unknown"
    return HermesCheckout(src, ref, out, sha)


# Hermes classes the plugin wraps (class attributes, process-wide). Tests in this directory share one
# process: a wrapper installed by one test's (later evicted) runtime must not leak into the next test.
_WRAPPED_TARGETS = (
    ("gateway.run", "GatewayRunner", ("_handle_message", "_is_user_authorized", "_resolve_session_agent_runtime",
                                      "_fallback_baseline_model")),
    ("plugins.platforms.discord.adapter", "DiscordAdapter",
     ("_discord_message_admission", "_handle_message", "delete_message")),
    ("gateway.run_turn_runner", "TurnRunner", ("_progress_absorb", "_progress_edit_state")),
)


def _strip_plugin_wrappers() -> None:
    for module_name, class_name, names in _WRAPPED_TARGETS:
        cls = getattr(sys.modules.get(module_name), class_name, None)
        if cls is None:
            continue
        for name in names:
            current = cls.__dict__.get(name)
            if current is None or getattr(current, "__dsp_wrapper__", None) is None:
                continue
            original = current
            while getattr(original, "__dsp_wrapper__", None) is not None:
                original = getattr(original, "__wrapped__", None)
            inherited = next((base.__dict__[name] for base in cls.__mro__[1:] if name in base.__dict__), None)
            if original is None or original is inherited:
                delattr(cls, name)
            else:
                setattr(cls, name, original)


@pytest.fixture(autouse=True)
def _pristine_hermes_classes():
    if HERMES_SRC:
        _strip_plugin_wrappers()
    yield
    if HERMES_SRC:
        _strip_plugin_wrappers()
